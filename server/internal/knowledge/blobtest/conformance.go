// Package blobtest provides the reusable blob Store conformance suite.
package blobtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

const concurrentWriters = 2

type suite struct {
	store blob.Store
	root  string
}

// RunConformance exercises Store behavior under an unused key prefix.
func RunConformance(t *testing.T, store blob.Store, prefix string) {
	t.Helper()
	if store == nil {
		t.Fatal("blobtest: nil store")
	}
	root := prefix + "blobtest/"
	if err := blob.ValidatePrefix(root); err != nil {
		t.Fatalf("blobtest prefix: %v", err)
	}
	tests := []struct {
		name string
		run  func(*testing.T, suite)
	}{
		{"RoundTrips", testRoundTrips},
		{"Aliasing", testAliasing},
		{"DuplicateCreate", testDuplicateCreate},
		{"ReplaceCAS", testReplaceCAS},
		{"DeleteCAS", testDeleteCAS},
		{"DeleteRecreateABA", testDeleteRecreateABA},
		{"ConcurrentCreate", testConcurrentCreate},
		{"ConcurrentReplace", testConcurrentReplace},
		{"List", testList},
		{"Validation", testValidation},
		{"CanceledContext", testCanceledContext},
		{"ZeroResults", testZeroResults},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, suite{store: store, root: root + test.name + "/"})
		})
	}
}

func testRoundTrips(t *testing.T, s suite) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"nonempty", []byte("object data")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := s.key(test.name)
			attributes := mustCreate(t, s.store, key, test.data)
			assertAttributes(t, attributes, key, len(test.data))

			object, err := s.store.Get(context.Background(), key)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if !bytes.Equal(object.Data, test.data) {
				t.Errorf("data = %q, want %q", object.Data, test.data)
			}
			if object.Attributes != attributes {
				t.Errorf("get attributes = %+v, want %+v", object.Attributes, attributes)
			}
			head, err := s.store.Head(context.Background(), key)
			if err != nil {
				t.Fatalf("head: %v", err)
			}
			if head != attributes {
				t.Errorf("head attributes = %+v, want %+v", head, attributes)
			}
		})
	}
}

func testAliasing(t *testing.T, s suite) {
	key := s.key("object")
	input := []byte("created")
	created := mustCreate(t, s.store, key, input)
	input[0] = 'X'
	assertData(t, s.store, key, "created")

	object, err := s.store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	object.Data[0] = 'Y'
	object.Attributes.Key = "changed"
	assertData(t, s.store, key, "created")

	replacement := []byte("replaced")
	replaced, err := s.store.Replace(context.Background(), key, created.Generation, replacement)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	replacement[0] = 'Z'
	assertData(t, s.store, key, "replaced")

	result, err := s.store.List(context.Background(), s.root, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(result.Objects) != 1 || result.Objects[0] != replaced {
		t.Fatalf("list = %+v, want replaced attributes", result)
	}
	result.Objects[0].Key = "changed"
	again, err := s.store.List(context.Background(), s.root, "")
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	if len(again.Objects) != 1 || again.Objects[0] != replaced {
		t.Errorf("list after output mutation = %+v, want %+v", again.Objects, replaced)
	}
}

func testDuplicateCreate(t *testing.T, s suite) {
	key := s.key("object")
	created := mustCreate(t, s.store, key, []byte("original"))
	attributes, err := s.store.Create(context.Background(), key, []byte("duplicate"))
	requireOpError(t, err, blob.ErrPrecondition, "create", key)
	assertZeroAttributes(t, attributes)
	object := mustGet(t, s.store, key)
	if string(object.Data) != "original" || object.Attributes != created {
		t.Errorf("object after duplicate = %+v, want original %+v", object, created)
	}
}

func testReplaceCAS(t *testing.T, s suite) {
	key := s.key("object")
	created := mustCreate(t, s.store, key, []byte("v1"))
	replaced, err := s.store.Replace(context.Background(), key, created.Generation, []byte("v2"))
	if err != nil {
		t.Fatalf("matching replace: %v", err)
	}
	if replaced.Generation == created.Generation {
		t.Errorf("replacement reused generation %d", replaced.Generation)
	}
	assertAttributes(t, replaced, key, 2)

	attributes, err := s.store.Replace(context.Background(), key, created.Generation, []byte("old"))
	requireOpError(t, err, blob.ErrPrecondition, "replace", key)
	assertZeroAttributes(t, attributes)
	object := mustGet(t, s.store, key)
	if string(object.Data) != "v2" || object.Attributes != replaced {
		t.Errorf("object after stale replace = %+v, want v2 %+v", object, replaced)
	}

	missing := s.key("missing")
	attributes, err = s.store.Replace(context.Background(), missing, replaced.Generation, []byte("data"))
	requireOpError(t, err, blob.ErrPrecondition, "replace", missing)
	assertZeroAttributes(t, attributes)
}

func testDeleteCAS(t *testing.T, s suite) {
	key := s.key("object")
	created := mustCreate(t, s.store, key, []byte("data"))
	replaced, err := s.store.Replace(context.Background(), key, created.Generation, []byte("data"))
	if err != nil {
		t.Fatalf("replace before stale delete: %v", err)
	}
	if err := s.store.Delete(context.Background(), key, created.Generation); err == nil {
		t.Fatal("stale delete succeeded")
	} else {
		requireOpError(t, err, blob.ErrPrecondition, "delete", key)
	}
	assertData(t, s.store, key, "data")
	if err := s.store.Delete(context.Background(), key, replaced.Generation); err != nil {
		t.Fatalf("matching delete: %v", err)
	}
	object, err := s.store.Get(context.Background(), key)
	requireOpError(t, err, blob.ErrNotFound, "get", key)
	assertZeroObject(t, &object)
	if err := s.store.Delete(context.Background(), key, replaced.Generation); err == nil {
		t.Fatal("missing delete succeeded")
	} else {
		requireOpError(t, err, blob.ErrPrecondition, "delete", key)
	}
}

func testDeleteRecreateABA(t *testing.T, s suite) {
	key := s.key("object")
	first := mustCreate(t, s.store, key, []byte("first"))
	if err := s.store.Delete(context.Background(), key, first.Generation); err != nil {
		t.Fatalf("delete first: %v", err)
	}
	second := mustCreate(t, s.store, key, []byte("second"))
	if second.Generation == first.Generation {
		t.Fatalf("recreated object reused generation %d", second.Generation)
	}
	attributes, err := s.store.Replace(context.Background(), key, first.Generation, []byte("stale"))
	requireOpError(t, err, blob.ErrPrecondition, "replace", key)
	assertZeroAttributes(t, attributes)
	if err := s.store.Delete(context.Background(), key, first.Generation); err == nil {
		t.Fatal("ABA delete succeeded")
	} else {
		requireOpError(t, err, blob.ErrPrecondition, "delete", key)
	}
	object := mustGet(t, s.store, key)
	if string(object.Data) != "second" || object.Attributes != second {
		t.Errorf("recreated object = %+v, want second %+v", object, second)
	}
}

func testConcurrentCreate(t *testing.T, s suite) {
	key := s.key("object")
	results := runConcurrentWrites(func(index int) (blob.Attributes, error) {
		return s.store.Create(context.Background(), key, fmt.Appendf(nil, "writer-%d", index))
	})
	winner := requireOneWinner(t, results, "create", key)
	object := mustGet(t, s.store, key)
	wantData := fmt.Sprintf("writer-%d", winner.index)
	if string(object.Data) != wantData || object.Attributes != winner.attributes {
		t.Errorf("winning object = %+v, want data %q attributes %+v", object, wantData, winner.attributes)
	}
}

func testConcurrentReplace(t *testing.T, s suite) {
	key := s.key("object")
	created := mustCreate(t, s.store, key, []byte("base"))
	results := runConcurrentWrites(func(index int) (blob.Attributes, error) {
		return s.store.Replace(context.Background(), key, created.Generation, fmt.Appendf(nil, "writer-%d", index))
	})
	winner := requireOneWinner(t, results, "replace", key)
	object := mustGet(t, s.store, key)
	wantData := fmt.Sprintf("writer-%d", winner.index)
	if string(object.Data) != wantData || object.Attributes != winner.attributes {
		t.Errorf("winning object = %+v, want data %q attributes %+v", object, wantData, winner.attributes)
	}
}

func testList(t *testing.T, s suite) {
	keys := []string{s.key("z"), s.key("a"), s.key("m")}
	current := make(map[string]blob.Attributes, len(keys))
	for _, key := range keys {
		current[key] = mustCreate(t, s.store, key, []byte(key))
	}
	replaced, err := s.store.Replace(context.Background(), keys[2], current[keys[2]].Generation, []byte("current"))
	if err != nil {
		t.Fatalf("replace listed object: %v", err)
	}
	current[keys[2]] = replaced
	mustCreate(t, s.store, strings.TrimSuffix(s.root, "/")+"-outside", nil)

	result, err := s.store.List(context.Background(), s.root, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if result.NextCursor != "" {
		t.Errorf("next cursor = %q, want empty", result.NextCursor)
	}
	wantKeys := slices.Clone(keys)
	slices.Sort(wantKeys)
	gotKeys := make([]string, len(result.Objects))
	for index, attributes := range result.Objects {
		gotKeys[index] = attributes.Key
		if attributes != current[attributes.Key] {
			t.Errorf("attributes for %q = %+v, want %+v", attributes.Key, attributes, current[attributes.Key])
		}
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("listed keys = %q, want %q", gotKeys, wantKeys)
	}
}

func testValidation(t *testing.T, s suite) {
	invalidKeys := []string{"", ".", "..", "line\nbreak", "line\rbreak", string([]byte{0xff}), strings.Repeat("k", blob.MaxKeyBytes+1)}
	for _, key := range invalidKeys {
		attributes, err := s.store.Create(context.Background(), key, nil)
		requireOpError(t, err, blob.ErrPrecondition, "create", key)
		assertZeroAttributes(t, attributes)
	}

	key := s.key("generation")
	created := mustCreate(t, s.store, key, []byte("original"))
	for _, generation := range []blob.Generation{0, -1} {
		attributes, err := s.store.Replace(context.Background(), key, generation, []byte("invalid"))
		requireOpError(t, err, blob.ErrPrecondition, "replace", key)
		assertZeroAttributes(t, attributes)
		requireOpError(t, s.store.Delete(context.Background(), key, generation), blob.ErrPrecondition, "delete", key)
	}
	object := mustGet(t, s.store, key)
	if string(object.Data) != "original" || object.Attributes != created {
		t.Errorf("object after invalid generations = %+v, want original %+v", object, created)
	}

	invalidPrefixes := []string{"line\nbreak", "line\rbreak", string([]byte{0xff}), strings.Repeat("p", blob.MaxKeyBytes+1)}
	for _, prefix := range invalidPrefixes {
		result, err := s.store.List(context.Background(), prefix, "")
		requireOpError(t, err, blob.ErrPrecondition, "list", prefix)
		assertZeroListResult(t, result)
	}
	for _, prefix := range []string{".", ".."} {
		if _, err := s.store.List(context.Background(), prefix, ""); err != nil {
			t.Errorf("valid prefix %q: %v", prefix, err)
		}
	}
	result, err := s.store.List(context.Background(), s.root, "not-a-cursor")
	requireOpError(t, err, blob.ErrPrecondition, "list", s.root)
	assertZeroListResult(t, result)
}

func testCanceledContext(t *testing.T, s suite) {
	key := s.key("existing")
	created := mustCreate(t, s.store, key, []byte("original"))
	newKey := s.key("new")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	object, err := s.store.Get(ctx, key)
	requireOpError(t, err, context.Canceled, "get", key)
	assertZeroObject(t, &object)
	attributes, err := s.store.Head(ctx, key)
	requireOpError(t, err, context.Canceled, "head", key)
	assertZeroAttributes(t, attributes)
	attributes, err = s.store.Create(ctx, newKey, []byte("new"))
	requireOpError(t, err, context.Canceled, "create", newKey)
	assertZeroAttributes(t, attributes)
	attributes, err = s.store.Replace(ctx, key, created.Generation, []byte("replaced"))
	requireOpError(t, err, context.Canceled, "replace", key)
	assertZeroAttributes(t, attributes)
	requireOpError(t, s.store.Delete(ctx, key, created.Generation), context.Canceled, "delete", key)
	result, err := s.store.List(ctx, s.root, "")
	requireOpError(t, err, context.Canceled, "list", s.root)
	assertZeroListResult(t, result)

	object = mustGet(t, s.store, key)
	if string(object.Data) != "original" || object.Attributes != created {
		t.Errorf("object after canceled writes = %+v, want original %+v", object, created)
	}
	object, err = s.store.Get(context.Background(), newKey)
	requireOpError(t, err, blob.ErrNotFound, "get", newKey)
	assertZeroObject(t, &object)
}

func testZeroResults(t *testing.T, s suite) {
	missing := s.key("missing")
	object, err := s.store.Get(context.Background(), missing)
	requireOpError(t, err, blob.ErrNotFound, "get", missing)
	assertZeroObject(t, &object)
	attributes, err := s.store.Head(context.Background(), missing)
	requireOpError(t, err, blob.ErrNotFound, "head", missing)
	assertZeroAttributes(t, attributes)
	attributes, err = s.store.Replace(context.Background(), missing, 1, nil)
	requireOpError(t, err, blob.ErrPrecondition, "replace", missing)
	assertZeroAttributes(t, attributes)
	result, err := s.store.List(context.Background(), s.root, "bad cursor")
	requireOpError(t, err, blob.ErrPrecondition, "list", s.root)
	assertZeroListResult(t, result)
}

type writeResult struct {
	index      int
	attributes blob.Attributes
	err        error
}

func runConcurrentWrites(write func(int) (blob.Attributes, error)) []writeResult {
	start := make(chan struct{})
	results := make(chan writeResult, concurrentWriters)
	var wait sync.WaitGroup
	for index := range concurrentWriters {
		wait.Go(func() {
			<-start
			attributes, err := write(index)
			results <- writeResult{index: index, attributes: attributes, err: err}
		})
	}
	close(start)
	wait.Wait()
	close(results)
	all := make([]writeResult, 0, concurrentWriters)
	for result := range results {
		all = append(all, result)
	}
	return all
}

func requireOneWinner(t *testing.T, results []writeResult, op, key string) writeResult {
	t.Helper()
	winners := make([]writeResult, 0, 1)
	for _, result := range results {
		if result.err == nil {
			winners = append(winners, result)
			continue
		}
		requireOpError(t, result.err, blob.ErrPrecondition, op, key)
		assertZeroAttributes(t, result.attributes)
	}
	if len(winners) != 1 {
		t.Fatalf("successful %s writers = %d, want 1; results: %+v", op, len(winners), results)
	}
	return winners[0]
}

func (s suite) key(suffix string) string {
	return s.root + suffix
}

func mustCreate(t *testing.T, store blob.Store, key string, data []byte) blob.Attributes {
	t.Helper()
	attributes, err := store.Create(context.Background(), key, data)
	if err != nil {
		t.Fatalf("create %q: %v", key, err)
	}
	return attributes
}

func mustGet(t *testing.T, store blob.Store, key string) blob.Object {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return object
}

func assertData(t *testing.T, store blob.Store, key, want string) {
	t.Helper()
	object := mustGet(t, store, key)
	if string(object.Data) != want {
		t.Errorf("data = %q, want %q", object.Data, want)
	}
}

func assertAttributes(t *testing.T, attributes blob.Attributes, key string, size int) {
	t.Helper()
	if attributes.Key != key || attributes.Size != int64(size) || attributes.Generation <= 0 {
		t.Errorf("attributes = %+v, want key %q size %d positive generation", attributes, key, size)
	}
	if attributes.Modified.IsZero() {
		t.Error("modified is zero")
	}
}

func requireOpError(t *testing.T, err, target error, op, key string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
	var operation *blob.OpError
	if !errors.As(err, &operation) {
		t.Fatalf("error type = %T, want *blob.OpError", err)
	}
	if operation.Op != op || operation.Key != key {
		t.Errorf("operation error = %+v, want op %q key %q", operation, op, key)
	}
}

func assertZeroObject(t *testing.T, object *blob.Object) {
	t.Helper()
	if object.Data != nil || object.Attributes != (blob.Attributes{}) {
		t.Errorf("error object = %+v, want zero", object)
	}
}

func assertZeroAttributes(t *testing.T, attributes blob.Attributes) {
	t.Helper()
	if attributes != (blob.Attributes{}) {
		t.Errorf("error attributes = %+v, want zero", attributes)
	}
}

func assertZeroListResult(t *testing.T, result blob.ListResult) {
	t.Helper()
	if result.Objects != nil || result.NextCursor != "" {
		t.Errorf("error list result = %+v, want zero", result)
	}
}
