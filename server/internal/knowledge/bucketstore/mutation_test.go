package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

func TestConcurrentCASMutations(t *testing.T) {
	t.Run("same path conflicts", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/same", right, "/same")
		barrier.releaseBoth(t)
		outcomes := collectWriteOutcomes(t, results)
		assertOneConflict(t, outcomes)
		assertCurrentVersion(t, left, "/same", 1)
	})

	t.Run("unrelated paths rebase", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/left", right, "/right")
		barrier.releaseBoth(t)
		outcomes := collectWriteOutcomes(t, results)
		for _, outcome := range outcomes {
			if outcome.err != nil || outcome.document == nil || outcome.document.Version != 1 {
				t.Fatalf("write outcome = (%+v, %v), want v1 success", outcome.document, outcome.err)
			}
		}
		assertCurrentVersion(t, left, "/left", 1)
		assertCurrentVersion(t, left, "/right", 1)
		head := currentHead(t, barrier.Store)
		if head.Sequence != 3 || len(head.Receipts) != 2 {
			t.Errorf("head sequence=%d receipts=%d, want 3/2", head.Sequence, len(head.Receipts))
		}
	})

	t.Run("document directory race", func(t *testing.T) {
		left, right, barrier := concurrentStores(t)
		results := runConcurrentWrites(left, "/a", right, "/a/b")
		barrier.releaseBoth(t)
		outcomes := collectWriteOutcomes(t, results)
		succeeded := 0
		failed := 0
		for _, outcome := range outcomes {
			if outcome.err == nil {
				succeeded++
			} else {
				failed++
			}
		}
		if succeeded != 1 || failed != 1 {
			t.Fatalf("topology outcomes success=%d failure=%d, want 1/1", succeeded, failed)
		}
		leftVersion, err := left.CurrentVersion("/a")
		if err != nil {
			t.Fatalf("read /a current version: %v", err)
		}
		if leftVersion > 0 {
			assertCurrentVersion(t, left, "/a/b", 0)
		} else {
			assertCurrentVersion(t, left, "/a/b", 1)
		}
	})
}

func TestDelayedHeadReplaceAllowsChangedGenerationRead(t *testing.T) {
	_, memory := newWritableStore(t)
	delayed := newDelayedHeadReplaceStore(memory)
	defer delayed.releaseResponse()
	writer, err := Open(context.Background(), delayed, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.commitInterval = 0

	result := make(chan writeOutcome, 1)
	go func() {
		document, err := writer.WriteVersion("/candidate", 0, []byte("candidate"), nil)
		result <- writeOutcome{document: document, err: err}
	}()
	waitForTestSignal(t, delayed.committed, "committed delayed CAS")
	delayed.observeRoot.Store(true)
	viewResult := make(chan readViewResult, 1)
	go func() {
		view, err := writer.openReadView()
		viewResult <- readViewResult{view: view, err: err}
	}()
	waitForTestSignal(t, delayed.rootRead, "changed root read during delayed CAS")
	view := receiveReadView(t, viewResult)
	defer closeTestReadView(t, view)
	document, err := view.Get("/candidate", 0)
	if err != nil || document.Version != 1 {
		t.Fatalf("changed-generation read = (%+v, %v), want v1", document, err)
	}

	delayed.releaseResponse()
	outcome := receiveWriteOutcome(t, result)
	if outcome.err != nil || outcome.document == nil || outcome.document.Version != 1 {
		t.Fatalf("delayed write = (%+v, %v), want v1 success", outcome.document, outcome.err)
	}
}

func TestDelayedHeadReplaceDoesNotOverwriteNewerSnapshot(t *testing.T) {
	_, memory := newWritableStore(t)
	delayed := newDelayedHeadReplaceStore(memory)
	defer delayed.releaseResponse()
	writer, err := Open(context.Background(), delayed, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.commitInterval = 0
	peer, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}
	peer.commitInterval = 0

	result := make(chan writeOutcome, 1)
	go func() {
		document, err := writer.WriteVersion("/candidate", 0, []byte("candidate"), nil)
		result <- writeOutcome{document: document, err: err}
	}()
	waitForTestSignal(t, delayed.committed, "committed delayed CAS")
	first, err := writer.openReadView()
	if err != nil {
		t.Fatalf("refresh candidate: %v", err)
	}
	closeTestReadView(t, first)
	if _, err := peer.WriteVersion("/newer", 0, []byte("newer"), nil); err != nil {
		t.Fatalf("commit newer snapshot: %v", err)
	}
	newer, err := writer.openReadView()
	if err != nil {
		t.Fatalf("refresh newer snapshot: %v", err)
	}
	closeTestReadView(t, newer)
	wantGeneration := getObject(t, memory, headObjectKey).Attributes.Generation

	delayed.releaseResponse()
	outcome := receiveWriteOutcome(t, result)
	if outcome.err != nil || outcome.document == nil || outcome.document.Version != 1 {
		t.Fatalf("delayed write = (%+v, %v), want committed success", outcome.document, outcome.err)
	}
	installed := writer.snapshot.Load()
	if installed.HeadGeneration != wantGeneration || installed.Paths["/newer"].Current != 1 {
		t.Fatalf("installed snapshot generation=%d paths=%v, want newer generation %d", installed.HeadGeneration, installed.Paths, wantGeneration)
	}
}

func TestProductionWritePinsOldReadView(t *testing.T) {
	store, _ := newWritableStore(t)
	if _, err := store.WriteVersion("/doc", 0, []byte("v1"), nil); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	old, err := store.openReadView()
	if err != nil {
		t.Fatalf("open old view: %v", err)
	}
	defer closeTestReadView(t, old)
	if _, err := store.WriteVersion("/doc", 1, []byte("v2"), nil); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	oldDocument, err := old.Get("/doc", 0)
	if err != nil || string(oldDocument.Content) != "v1" || oldDocument.Version != 1 {
		t.Errorf("old view = (%+v, %v), want v1", oldDocument, err)
	}
	fresh, err := store.Get("/doc", 0)
	if err != nil || string(fresh.Content) != "v2" || fresh.Version != 2 {
		t.Errorf("fresh view = (%+v, %v), want v2", fresh, err)
	}
}

func TestArchiveRebaseReturnsCommittedVersion(t *testing.T) {
	base, memory := newWritableStore(t)
	if _, err := base.WriteVersion("/doc", 0, []byte("v1"), nil); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	blocking := &blockingFirstHeadReplaceStore{
		Store:   memory,
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
	archiver, err := Open(context.Background(), blocking, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open archiver: %v", err)
	}
	archiver.commitInterval = 0
	writer, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.commitInterval = 0
	type archiveOutcome struct {
		document *protocolstore.Document
		changed  bool
		err      error
	}
	result := make(chan archiveOutcome, 1)
	go func() {
		document, changed, err := archiver.Archive("/doc", true)
		result <- archiveOutcome{document: document, changed: changed, err: err}
	}()
	waitForTestSignal(t, blocking.blocked, "blocked archive CAS")
	if _, err := writer.WriteVersion("/doc", 1, []byte("v2"), nil); err != nil {
		t.Fatalf("write concurrent v2: %v", err)
	}
	close(blocking.release)
	select {
	case outcome := <-result:
		if outcome.err != nil || !outcome.changed || outcome.document == nil || outcome.document.Version != 2 || !outcome.document.Archived {
			t.Fatalf("archive outcome = (%+v, changed=%v, err=%v), want archived v2", outcome.document, outcome.changed, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for archive rebase")
	}
}

func TestArchiveNoOpDoesNotCommit(t *testing.T) {
	store, memory := newWritableStore(t)
	if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	archived, changed, err := store.Archive("/doc", true)
	if err != nil || !changed {
		t.Fatalf("archive = (%+v, %v, %v)", archived, changed, err)
	}
	before := currentHead(t, memory)
	unchanged, changed, err := store.Archive("/doc", true)
	if err != nil || changed || unchanged.ETag != archived.ETag || !unchanged.Modified.Equal(archived.Modified) {
		t.Fatalf("archive no-op = (%+v, %v, %v)", unchanged, changed, err)
	}
	after := currentHead(t, memory)
	if after.Sequence != before.Sequence || len(after.Receipts) != len(before.Receipts) {
		t.Errorf("no-op changed head from sequence %d/%d receipts to %d/%d", before.Sequence, len(before.Receipts), after.Sequence, len(after.Receipts))
	}
}

func TestPublishPolicyCAS(t *testing.T) {
	t.Run("warn block and ask", func(t *testing.T) {
		store, _ := newWritableStore(t)
		seedPolicy(t, store, "strictness: warn\nrequire_tags: domain\nrequire_fields: title\n", 0)
		warned, err := store.WriteVersionResult("/warned", 0, []byte("body"), nil)
		if err != nil || warned.Strictness != publishpolicy.Warn || warned.Policy.Compliant() {
			t.Fatalf("warn result = (%+v, %v)", warned, err)
		}
		seedPolicy(t, store, "strictness: block\nrequire_tags: domain\nrequire_fields: title\n", 1)
		blocked, err := store.WriteVersionResult("/blocked", 0, []byte("body"), nil)
		if !errors.Is(err, ErrPolicyBlocked) || blocked.Policy.Compliant() {
			t.Fatalf("block result = (%+v, %v)", blocked, err)
		}
		compliant := map[string]string{"tags": "domain:storage", "title": "Allowed"}
		if _, err := store.WriteVersion("/allowed", 0, []byte("body"), compliant); err != nil {
			t.Fatalf("compliant write: %v", err)
		}
		seedPolicy(t, store, "strictness: ask\nrequire_tags: domain\nrequire_fields: title\n", 2)
		_, err = store.WriteVersion("/asked", 0, []byte("body"), nil)
		if !errors.Is(err, ErrPolicyApprovalRequired) {
			t.Fatalf("ask error = %v", err)
		}
	})

	t.Run("append evaluates final metadata", func(t *testing.T) {
		store, _ := newWritableStore(t)
		seedPolicy(t, store, "strictness: block\nrequire_tags: domain\nrequire_fields: title\n", 0)
		meta := map[string]string{"tags": "domain:storage", "title": "Log"}
		if _, err := store.WriteVersion("/log", 0, []byte("one"), meta); err != nil {
			t.Fatalf("seed document: %v", err)
		}
		if _, err := store.Append("/log", 1, []byte("two"), nil); err != nil {
			t.Fatalf("inherited append: %v", err)
		}
		if _, err := store.Append("/log", 2, []byte("three"), map[string]string{"tags": "other:value"}); !errors.Is(err, ErrPolicyBlocked) {
			t.Fatalf("noncompliant append error = %v", err)
		}
	})

	t.Run("invalid and required policy", func(t *testing.T) {
		store, _ := newWritableStore(t)
		if _, err := store.WriteVersion(publishpolicy.DocumentPath, 0, []byte("strictness: invalid\n"), policyMetadata()); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("invalid candidate policy error = %v", err)
		}
		store.requirePolicy = true
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), policyMetadata()); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("required missing policy error = %v", err)
		}
	})

	t.Run("rebase reevaluates changed policy", func(t *testing.T) {
		base, memory := newWritableStore(t)
		seedPolicy(t, base, "strictness: warn\nrequire_tags: domain\nrequire_fields: title\n", 0)
		blocking := &blockingFirstHeadReplaceStore{
			Store:   memory,
			blocked: make(chan struct{}),
			release: make(chan struct{}),
		}
		writer, err := Open(context.Background(), blocking, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open writer: %v", err)
		}
		writer.commitInterval = 0
		policyWriter, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open policy writer: %v", err)
		}
		policyWriter.commitInterval = 0
		result := make(chan error, 1)
		go func() {
			_, err := writer.WriteVersion("/doc", 0, []byte("body"), nil)
			result <- err
		}()
		waitForTestSignal(t, blocking.blocked, "blocked document CAS")
		seedPolicy(t, policyWriter, "strictness: block\nrequire_tags: domain\nrequire_fields: title\n", 1)
		close(blocking.release)
		select {
		case err := <-result:
			if !errors.Is(err, ErrPolicyBlocked) {
				t.Fatalf("rebased write error = %v, want policy block", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for rebased policy result")
		}
		assertCurrentVersion(t, policyWriter, "/doc", 0)
	})
}

func TestHistoryBlockRolloverAndRetention(t *testing.T) {
	store, _ := newWritableStore(t)
	var document *protocolstore.Document
	for version := 1; version <= 256; version++ {
		var err error
		document, err = store.WriteVersion("/history", version-1, fmt.Appendf(nil, "v%d", version), nil)
		if err != nil {
			t.Fatalf("write v%d: %v", version, err)
		}
	}
	if document.Version != 256 {
		t.Fatalf("tip = v%d, want v256", document.Version)
	}
	before := currentManifest(t, store, "/history")
	if len(before.History) != 1 || before.History[0].First != 1 || before.History[0].Last != 256 {
		t.Fatalf("v256 history refs = %+v", before.History)
	}
	fullBlock := before.History[0]
	if _, err := store.WriteVersion("/history", 256, []byte("v257"), nil); err != nil {
		t.Fatalf("write v257: %v", err)
	}
	rolled := currentManifest(t, store, "/history")
	if len(rolled.History) != 2 || rolled.History[0] != fullBlock || rolled.History[1].First != 257 || rolled.History[1].Last != 257 {
		t.Fatalf("v257 history refs = %+v", rolled.History)
	}
	pruned, err := store.WriteVersion("/history", 257, []byte("v258"), map[string]string{"retention": "5"})
	if err != nil {
		t.Fatalf("write retained v258: %v", err)
	}
	if pruned.Prune == nil || pruned.Prune.From != 1 || pruned.Prune.To != 253 {
		t.Fatalf("prune = %+v, want 1-253", pruned.Prune)
	}
	trimmed := currentManifest(t, store, "/history")
	if len(trimmed.History) != 2 || trimmed.History[0].First != 254 || trimmed.History[0].Last != 256 || trimmed.History[1].First != 257 || trimmed.History[1].Last != 258 {
		t.Fatalf("trimmed history refs = %+v", trimmed.History)
	}
	versions, err := store.Versions("/history")
	if err != nil || len(versions) != 5 || versions[0].Version != 258 || versions[4].Version != 254 {
		t.Fatalf("retained versions = (%+v, %v)", versions, err)
	}
	if err := store.VerifyChain("/history"); err != nil {
		t.Fatalf("verify retained chain: %v", err)
	}
	head := store.snapshot.Load().Head
	if len(head.Receipts) != maximumReceipts || head.Receipts[0].Sequence != head.Sequence-int64(maximumReceipts)+1 {
		t.Errorf("receipt window at sequence %d = %+v", head.Sequence, head.Receipts)
	}
}

func TestHeadOutcomeReconciliation(t *testing.T) {
	tests := []struct {
		name      string
		wrap      func(blob.Store) blob.Store
		wantError string
	}{
		{name: "ambiguous before commit", wrap: func(store blob.Store) blob.Store {
			return &failFirstHeadReplaceStore{Store: store, category: blob.ErrAmbiguous}
		}},
		{name: "throttled before commit", wrap: func(store blob.Store) blob.Store {
			return &failFirstHeadReplaceStore{Store: store, category: errors.Join(blob.ErrThrottled, blob.ErrAmbiguous)}
		}},
		{name: "ambiguous after commit", wrap: func(store blob.Store) blob.Store {
			return &ambiguousCommittedHeadStore{Store: store}
		}},
		{name: "receipt evicted", wrap: func(store blob.Store) blob.Store {
			return &evictingHeadStore{Store: store}
		}, wantError: "evicted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, memory := newWritableStore(t)
			store.objects = test.wrap(memory)
			document, err := store.WriteVersion("/doc", 0, []byte("body"), nil)
			if test.wantError == "" {
				if err != nil || document == nil || document.Version != 1 {
					t.Fatalf("write = (%+v, %v), want v1", document, err)
				}
				assertCurrentVersion(t, store, "/doc", 1)
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("write error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestMutationFailureAndPacing(t *testing.T) {
	t.Run("immutable failure leaves head unchanged", func(t *testing.T) {
		store, memory := newWritableStore(t)
		before := currentHead(t, memory)
		store.objects = &failFirstBlobCreateStore{Store: memory}
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); err == nil {
			t.Fatal("write succeeded through permanent blob failure")
		}
		after := currentHead(t, memory)
		if after.Sequence != before.Sequence || after.Root != before.Root {
			t.Errorf("head changed after staging failure: before=%+v after=%+v", before, after)
		}
		store.objects = memory
		assertCurrentVersion(t, store, "/doc", 0)
	})

	t.Run("request cancellation", func(t *testing.T) {
		store, memory := newWritableStore(t)
		store.requestTimeout = 25 * time.Millisecond
		store.objects = &blockingReplaceStore{Store: memory}
		if _, err := store.WriteVersion("/doc", 0, []byte("body"), nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("write error = %v, want deadline", err)
		}
		if currentHead(t, memory).Sequence != 1 {
			t.Error("canceled replace changed head")
		}
	})

	t.Run("local pacing", func(t *testing.T) {
		store, _ := newWritableStore(t)
		store.commitInterval = 30 * time.Millisecond
		started := time.Now()
		if _, err := store.WriteVersion("/one", 0, []byte("one"), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteVersion("/two", 0, []byte("two"), nil); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed < 25*time.Millisecond {
			t.Errorf("two commits took %v, want pacing delay", elapsed)
		}
	})
}

type writeOutcome struct {
	document *protocolstore.Document
	err      error
}

func runConcurrentWrites(left *Store, leftPath string, right *Store, rightPath string) <-chan writeOutcome {
	results := make(chan writeOutcome, 2)
	go func() {
		document, err := left.WriteVersion(leftPath, 0, []byte("left"), nil)
		results <- writeOutcome{document: document, err: err}
	}()
	go func() {
		document, err := right.WriteVersion(rightPath, 0, []byte("right"), nil)
		results <- writeOutcome{document: document, err: err}
	}()
	return results
}

func collectWriteOutcomes(t *testing.T, results <-chan writeOutcome) []writeOutcome {
	t.Helper()
	outcomes := make([]writeOutcome, 0, 2)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case outcome := <-results:
			outcomes = append(outcomes, outcome)
		case <-timer.C:
			t.Fatal("timed out waiting for concurrent writes")
		}
	}
	return outcomes
}

func receiveWriteOutcome(t *testing.T, results <-chan writeOutcome) writeOutcome {
	t.Helper()
	select {
	case outcome := <-results:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for write")
		return writeOutcome{}
	}
}

func assertOneConflict(t *testing.T, outcomes []writeOutcome) {
	t.Helper()
	succeeded := 0
	conflicted := 0
	for _, outcome := range outcomes {
		switch {
		case outcome.err == nil:
			succeeded++
		case errors.Is(outcome.err, protocolstore.ErrConflict):
			conflicted++
		default:
			t.Errorf("unexpected write error: %v", outcome.err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Errorf("outcomes success=%d conflict=%d, want 1/1", succeeded, conflicted)
	}
}

func assertCurrentVersion(t *testing.T, store *Store, path string, want int) {
	t.Helper()
	got, err := store.CurrentVersion(path)
	if err != nil || got != want {
		t.Fatalf("CurrentVersion(%s) = (%d, %v), want (%d, nil)", path, got, err, want)
	}
}

func seedPolicy(t *testing.T, store *Store, body string, expected int) {
	t.Helper()
	if _, err := store.WriteVersion(publishpolicy.DocumentPath, expected, []byte(body), policyMetadata()); err != nil {
		t.Fatalf("write policy v%d: %v", expected+1, err)
	}
}

func policyMetadata() map[string]string {
	return map[string]string{"tags": "domain:policy", "title": "Policy"}
}

func currentManifest(t *testing.T, store *Store, path string) manifestObject {
	t.Helper()
	loaded := store.snapshot.Load()
	entry, exists := loaded.Paths[path]
	if !exists {
		t.Fatalf("path %s is missing", path)
	}
	view := &readView{ctx: context.Background(), cancel: func() {}, objects: store.objects, snapshot: loaded}
	history, err := view.loadHistory(&entry)
	if err != nil {
		t.Fatalf("load manifest %s: %v", path, err)
	}
	return history.manifest
}

func currentHead(t *testing.T, store blob.Store) headObject {
	t.Helper()
	object, err := store.Get(context.Background(), headObjectKey)
	if err != nil {
		t.Fatalf("get head: %v", err)
	}
	var head headObject
	if err := decodeImmutable(object.Data, &head); err != nil {
		t.Fatalf("decode head: %v", err)
	}
	return head
}

type replaceBarrierStore struct {
	blob.Store
	arrived chan struct{}
	release chan struct{}
	calls   atomic.Int64
	once    sync.Once
}

func concurrentStores(t *testing.T) (left, right *Store, barrier *replaceBarrierStore) {
	t.Helper()
	_, memory := newWritableStore(t)
	barrier = &replaceBarrierStore{Store: memory, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	open := func() *Store {
		store, err := Open(context.Background(), barrier, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open concurrent store: %v", err)
		}
		store.commitInterval = 0
		return store
	}
	return open(), open(), barrier
}

func (store *replaceBarrierStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	if key == headObjectKey && store.calls.Add(1) <= 2 {
		store.arrived <- struct{}{}
		select {
		case <-store.release:
		case <-ctx.Done():
			return blob.Attributes{}, &blob.OpError{Op: "replace", Key: key, Err: errors.Join(blob.ErrAmbiguous, ctx.Err())}
		}
	}
	return store.Store.Replace(ctx, key, generation, data)
}

func (store *replaceBarrierStore) releaseBoth(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-store.arrived:
		case <-timer.C:
			t.Fatal("timed out waiting for CAS contenders")
		}
	}
	store.once.Do(func() { close(store.release) })
}

type failFirstHeadReplaceStore struct {
	blob.Store
	failed   atomic.Bool
	category error
}

func (store *failFirstHeadReplaceStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	if key == headObjectKey && store.failed.CompareAndSwap(false, true) {
		return blob.Attributes{}, &blob.OpError{Op: "replace", Key: key, Err: store.category}
	}
	return store.Store.Replace(ctx, key, generation, data)
}

type ambiguousCommittedHeadStore struct {
	blob.Store
	failed atomic.Bool
}

func (store *ambiguousCommittedHeadStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	attributes, err := store.Store.Replace(ctx, key, generation, data)
	if err != nil || key != headObjectKey || !store.failed.CompareAndSwap(false, true) {
		return attributes, err
	}
	return attributes, &blob.OpError{Op: "replace", Key: key, Err: blob.ErrAmbiguous}
}

type evictingHeadStore struct {
	blob.Store
	failed atomic.Bool
}

func (store *evictingHeadStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	attributes, err := store.Store.Replace(ctx, key, generation, data)
	if err != nil || key != headObjectKey || !store.failed.CompareAndSwap(false, true) {
		return attributes, err
	}
	var head headObject
	if err := decodeImmutable(data, &head); err != nil {
		return blob.Attributes{}, err
	}
	current := attributes
	for index := range maximumReceipts + 1 {
		operationID := fmt.Sprintf("00000000-0000-4000-8001-%012x", index+1)
		head = nextHead(&head, head.Root, operationID)
		encoded, err := marshalImmutable(head)
		if err != nil {
			return blob.Attributes{}, err
		}
		current, err = store.Store.Replace(ctx, key, current.Generation, encoded)
		if err != nil {
			return blob.Attributes{}, err
		}
	}
	return attributes, &blob.OpError{Op: "replace", Key: key, Err: blob.ErrAmbiguous}
}

type failFirstBlobCreateStore struct {
	blob.Store
	failed atomic.Bool
}

func (store *failFirstBlobCreateStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if strings.HasPrefix(key, objectPrefix+"blobs/") && store.failed.CompareAndSwap(false, true) {
		return blob.Attributes{}, errors.New("injected permanent create failure")
	}
	return store.Store.Create(ctx, key, data)
}

type blockingReplaceStore struct{ blob.Store }

func (store *blockingReplaceStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	if key != headObjectKey {
		return store.Store.Replace(ctx, key, generation, data)
	}
	<-ctx.Done()
	return blob.Attributes{}, &blob.OpError{Op: "replace", Key: key, Err: errors.Join(blob.ErrAmbiguous, ctx.Err())}
}

type blockingFirstHeadReplaceStore struct {
	blob.Store
	blocked chan struct{}
	release chan struct{}
	once    atomic.Bool
}

type delayedHeadReplaceStore struct {
	blob.Store
	committed   chan struct{}
	release     chan struct{}
	rootRead    chan struct{}
	replaceOnce atomic.Bool
	rootOnce    atomic.Bool
	releaseOnce sync.Once
	observeRoot atomic.Bool
}

func newDelayedHeadReplaceStore(store blob.Store) *delayedHeadReplaceStore {
	return &delayedHeadReplaceStore{
		Store:     store,
		committed: make(chan struct{}),
		release:   make(chan struct{}),
		rootRead:  make(chan struct{}),
	}
}

func (store *delayedHeadReplaceStore) Get(ctx context.Context, key string) (blob.Object, error) {
	if store.observeRoot.Load() && strings.HasPrefix(key, objectPrefix+"roots/") && store.rootOnce.CompareAndSwap(false, true) {
		close(store.rootRead)
	}
	return store.Store.Get(ctx, key)
}

func (store *delayedHeadReplaceStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	attributes, err := store.Store.Replace(ctx, key, generation, data)
	if err != nil || key != headObjectKey || !store.replaceOnce.CompareAndSwap(false, true) {
		return attributes, err
	}
	close(store.committed)
	select {
	case <-store.release:
		return attributes, nil
	case <-ctx.Done():
		return attributes, &blob.OpError{Op: "replace", Key: key, Err: errors.Join(blob.ErrAmbiguous, ctx.Err())}
	}
}

func (store *delayedHeadReplaceStore) releaseResponse() {
	store.releaseOnce.Do(func() { close(store.release) })
}

func (store *blockingFirstHeadReplaceStore) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	if key == headObjectKey && store.once.CompareAndSwap(false, true) {
		close(store.blocked)
		select {
		case <-store.release:
		case <-ctx.Done():
			return blob.Attributes{}, &blob.OpError{Op: "replace", Key: key, Err: errors.Join(blob.ErrAmbiguous, ctx.Err())}
		}
	}
	return store.Store.Replace(ctx, key, generation, data)
}
