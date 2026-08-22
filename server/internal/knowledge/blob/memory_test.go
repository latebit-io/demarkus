package blob_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blobtest"
)

func TestMemoryConformance(t *testing.T) {
	store, err := blob.NewMemory(1 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	blobtest.RunConformance(t, store, "conformance/")
}

func TestMemoryPagination(t *testing.T) {
	store, err := blob.NewMemory(1)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	const count = blob.MaxListPage + 5
	const prefix = "pages/"
	want := make([]blob.Attributes, 0, count)
	for index := range count {
		key := fmt.Sprintf("%sitem-%04d", prefix, index)
		attributes, err := store.Create(context.Background(), key, nil)
		if err != nil {
			t.Fatalf("create %q: %v", key, err)
		}
		want = append(want, attributes)
	}
	if _, err := store.Create(context.Background(), "outside/item", nil); err != nil {
		t.Fatalf("create outside object: %v", err)
	}

	first, err := store.List(context.Background(), prefix, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Objects) != blob.MaxListPage || first.NextCursor == "" {
		t.Fatalf("first page = %d objects cursor %q", len(first.Objects), first.NextCursor)
	}
	if !slices.Equal(first.Objects, want[:blob.MaxListPage]) {
		t.Error("first page objects differ")
	}
	repeated, err := store.List(context.Background(), prefix, "")
	if err != nil {
		t.Fatalf("repeat first page: %v", err)
	}
	if repeated.NextCursor != first.NextCursor {
		t.Errorf("repeated cursor = %q, want %q", repeated.NextCursor, first.NextCursor)
	}
	second, err := store.List(context.Background(), prefix, first.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.NextCursor != "" || !slices.Equal(second.Objects, want[blob.MaxListPage:]) {
		t.Errorf("second page = %+v cursor %q", second.Objects, second.NextCursor)
	}
	wrongPrefix, err := store.List(context.Background(), "other/", first.NextCursor)
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("cursor under wrong prefix: %v", err)
	}
	if wrongPrefix.Objects != nil || wrongPrefix.NextCursor != "" {
		t.Errorf("wrong-prefix result = %+v, want zero", wrongPrefix)
	}
}

func TestMemoryDeterministicMetadata(t *testing.T) {
	stores := make([]*blob.Memory, 2)
	for index := range stores {
		store, err := blob.NewMemory(32)
		if err != nil {
			t.Fatalf("new memory %d: %v", index, err)
		}
		stores[index] = store
	}

	histories := make([][]blob.Attributes, len(stores))
	for index, store := range stores {
		first := mustCreate(t, store, "a", []byte("one"))
		second := mustCreate(t, store, "b", []byte("two"))
		third, err := store.Replace(context.Background(), "a", first.Generation, []byte("three"))
		if err != nil {
			t.Fatalf("replace store %d: %v", index, err)
		}
		if err := store.Delete(context.Background(), "b", second.Generation); err != nil {
			t.Fatalf("delete store %d: %v", index, err)
		}
		fourth := mustCreate(t, store, "b", []byte("four"))
		histories[index] = []blob.Attributes{first, second, third, fourth}
	}
	if !slices.Equal(histories[0], histories[1]) {
		t.Errorf("identical histories differ:\n%+v\n%+v", histories[0], histories[1])
	}
	for index, attributes := range histories[0] {
		if attributes.Generation <= 0 || attributes.Modified.Location() != time.UTC {
			t.Errorf("attributes %d = %+v, want positive generation and UTC", index, attributes)
		}
		wantModified := time.Unix(int64(attributes.Generation), 0).UTC()
		if attributes.Modified != wantModified {
			t.Errorf("modified %d = %v, want logical time %v", index, attributes.Modified, wantModified)
		}
		if index > 0 {
			previous := histories[0][index-1]
			if attributes.Generation <= previous.Generation || !attributes.Modified.After(previous.Modified) {
				t.Errorf("attributes %d = %+v, want after %+v", index, attributes, previous)
			}
		}
	}
}

func TestMemoryLimitsAndValidation(t *testing.T) {
	t.Run("constructor", func(t *testing.T) {
		for _, limit := range []int64{0, -1} {
			store, err := blob.NewMemory(limit)
			if !errors.Is(err, blob.ErrPrecondition) || store != nil {
				t.Errorf("NewMemory(%d) = (%v, %v), want nil ErrPrecondition", limit, store, err)
			}
		}
	})

	t.Run("write size", func(t *testing.T) {
		store, err := blob.NewMemory(3)
		if err != nil {
			t.Fatalf("new memory: %v", err)
		}
		attributes, err := store.Create(context.Background(), "large", []byte("four"))
		if !errors.Is(err, blob.ErrPrecondition) || attributes != (blob.Attributes{}) {
			t.Fatalf("oversized create = (%+v, %v)", attributes, err)
		}
		created := mustCreate(t, store, "exact", []byte("123"))
		attributes, err = store.Replace(context.Background(), "exact", created.Generation, []byte("four"))
		if !errors.Is(err, blob.ErrPrecondition) || attributes != (blob.Attributes{}) {
			t.Fatalf("oversized replace = (%+v, %v)", attributes, err)
		}
		object, err := store.Get(context.Background(), "exact")
		if err != nil || !bytes.Equal(object.Data, []byte("123")) || object.Attributes != created {
			t.Errorf("object after oversized replace = (%+v, %v)", object, err)
		}
	})

	t.Run("key byte limit", func(t *testing.T) {
		store, err := blob.NewMemory(1)
		if err != nil {
			t.Fatalf("new memory: %v", err)
		}
		exact := strings.Repeat("k", blob.MaxKeyBytes)
		if _, err := store.Create(context.Background(), exact, nil); err != nil {
			t.Fatalf("create maximum key: %v", err)
		}
		result, err := store.List(context.Background(), exact, "")
		if err != nil || len(result.Objects) != 1 || result.Objects[0].Key != exact {
			t.Errorf("list maximum prefix = (%+v, %v)", result, err)
		}
		tooLongRunes := strings.Repeat("é", blob.MaxKeyBytes/2+1)
		attributes, err := store.Create(context.Background(), tooLongRunes, nil)
		if !errors.Is(err, blob.ErrPrecondition) || attributes != (blob.Attributes{}) {
			t.Errorf("oversized UTF-8 key = (%+v, %v)", attributes, err)
		}
	})

	t.Run("empty prefix", func(t *testing.T) {
		store, err := blob.NewMemory(1)
		if err != nil {
			t.Fatalf("new memory: %v", err)
		}
		mustCreate(t, store, "b", nil)
		mustCreate(t, store, "a", nil)
		result, err := store.List(context.Background(), "", "")
		if err != nil {
			t.Fatalf("list empty prefix: %v", err)
		}
		if len(result.Objects) != 2 || result.Objects[0].Key != "a" || result.Objects[1].Key != "b" {
			t.Errorf("empty-prefix list = %+v", result.Objects)
		}
	})
}

func TestValidationFunctions(t *testing.T) {
	t.Run("keys", func(t *testing.T) {
		tests := []struct {
			key     string
			wantErr bool
		}{
			{key: "object"},
			{key: "./object"},
			{key: "", wantErr: true},
			{key: ".", wantErr: true},
			{key: "..", wantErr: true},
			{key: string([]byte{0xff}), wantErr: true},
			{key: strings.Repeat("k", blob.MaxKeyBytes+1), wantErr: true},
		}
		for _, test := range tests {
			t.Run(fmt.Sprintf("%q", test.key), func(t *testing.T) {
				err := blob.ValidateKey(test.key)
				if errors.Is(err, blob.ErrPrecondition) != test.wantErr {
					t.Errorf("ValidateKey(%q) = %v", test.key, err)
				}
			})
		}
	})

	t.Run("prefixes", func(t *testing.T) {
		tests := []struct {
			prefix  string
			wantErr bool
		}{
			{prefix: ""},
			{prefix: "."},
			{prefix: ".."},
			{prefix: strings.Repeat("p", blob.MaxKeyBytes)},
			{prefix: string([]byte{0xff}), wantErr: true},
			{prefix: strings.Repeat("p", blob.MaxKeyBytes+1), wantErr: true},
		}
		for _, test := range tests {
			t.Run(fmt.Sprintf("%q", test.prefix), func(t *testing.T) {
				err := blob.ValidatePrefix(test.prefix)
				if errors.Is(err, blob.ErrPrecondition) != test.wantErr {
					t.Errorf("ValidatePrefix(%q) = %v", test.prefix, err)
				}
			})
		}
	})
}

func TestCursorEnvelope(t *testing.T) {
	const prefix = "."
	const payload = "provider-token"
	cursor, err := blob.WrapCursor(prefix, payload)
	if err != nil {
		t.Fatalf("WrapCursor(): %v", err)
	}
	if cursor == "" || cursor == payload {
		t.Fatalf("wrapped cursor = %q", cursor)
	}
	unwrapped, err := blob.UnwrapCursor(prefix, cursor)
	if err != nil || unwrapped != payload {
		t.Fatalf("UnwrapCursor() = (%q, %v), want %q", unwrapped, err, payload)
	}

	t.Run("wrong prefix", func(t *testing.T) {
		unwrapped, err := blob.UnwrapCursor("other", cursor)
		if !errors.Is(err, blob.ErrPrecondition) || unwrapped != "" {
			t.Errorf("UnwrapCursor() = (%q, %v), want empty ErrPrecondition", unwrapped, err)
		}
	})

	t.Run("bounded payload", func(t *testing.T) {
		exact := strings.Repeat("x", blob.MaxCursorPayloadBytes)
		if _, err := blob.WrapCursor(prefix, exact); err != nil {
			t.Fatalf("maximum payload: %v", err)
		}
		cursor, err := blob.WrapCursor(prefix, exact+"x")
		if !errors.Is(err, blob.ErrPrecondition) || cursor != "" {
			t.Errorf("oversized payload = (%q, %v), want empty ErrPrecondition", cursor, err)
		}
	})

	t.Run("version", func(t *testing.T) {
		envelope, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		envelope[0]++
		unwrapped, err := blob.UnwrapCursor(prefix, base64.RawURLEncoding.EncodeToString(envelope))
		if !errors.Is(err, blob.ErrPrecondition) || unwrapped != "" {
			t.Errorf("unsupported version = (%q, %v), want empty ErrPrecondition", unwrapped, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		cursor, err := blob.WrapCursor(prefix, "")
		if err != nil || cursor != "" {
			t.Fatalf("empty WrapCursor() = (%q, %v)", cursor, err)
		}
		payload, err := blob.UnwrapCursor(prefix, "")
		if err != nil || payload != "" {
			t.Errorf("empty UnwrapCursor() = (%q, %v)", payload, err)
		}
	})
}

func TestOpError(t *testing.T) {
	err := &blob.OpError{Op: "get", Key: "key", Err: blob.ErrNotFound}
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("errors.Is(%v, ErrNotFound) = false", err)
	}
	if !strings.Contains(err.Error(), "get") || !strings.Contains(err.Error(), "key") {
		t.Errorf("Error() = %q, want operation and key", err.Error())
	}
}

func mustCreate(t *testing.T, store blob.Store, key string, data []byte) blob.Attributes {
	t.Helper()
	attributes, err := store.Create(context.Background(), key, data)
	if err != nil {
		t.Fatalf("create %q: %v", key, err)
	}
	return attributes
}
