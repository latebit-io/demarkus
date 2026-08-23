package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
)

const testWorldID = "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48"

func TestBootstrapSeedsAndVerifiesPolicy(t *testing.T) {
	t.Run("creates policy with required metadata", func(t *testing.T) {
		objects := newMemory(t)
		policy := []byte("# Policy\n\nstrictness: block\nrequire_tags: category\nrequire_fields: type\n")
		if err := bootstrap(context.Background(), objects, testWorldID, policy); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		store, err := bucketstore.Open(context.Background(), objects, bucketstore.Options{WorldID: testWorldID, RequirePolicy: true})
		if err != nil {
			t.Fatalf("open required policy: %v", err)
		}
		document, err := store.Get(publishpolicy.DocumentPath, 0)
		if err != nil {
			t.Fatalf("get policy: %v", err)
		}
		if !bytes.Equal(document.Content, policy) {
			t.Errorf("policy content = %q", document.Content)
		}
		for key, expected := range policyMetadata {
			if document.Metadata[key] != expected {
				t.Errorf("metadata[%q] = %q, want %q", key, document.Metadata[key], expected)
			}
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		objects := newMemory(t)
		policy := []byte("strictness: block\n")
		if err := bootstrap(context.Background(), objects, testWorldID, policy); err != nil {
			t.Fatalf("first bootstrap: %v", err)
		}
		if err := bootstrap(context.Background(), objects, testWorldID, policy); err != nil {
			t.Fatalf("second bootstrap: %v", err)
		}
		store, err := bucketstore.Open(context.Background(), objects, bucketstore.Options{WorldID: testWorldID, RequirePolicy: true})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		versions, err := store.Versions(publishpolicy.DocumentPath)
		if err != nil {
			t.Fatalf("versions: %v", err)
		}
		if len(versions) != 1 {
			t.Errorf("versions = %d, want 1", len(versions))
		}
	})

	t.Run("rejects differing policy without overwrite", func(t *testing.T) {
		objects := newMemory(t)
		original := []byte("strictness: warn\n")
		if err := bootstrap(context.Background(), objects, testWorldID, original); err != nil {
			t.Fatalf("first bootstrap: %v", err)
		}
		err := bootstrap(context.Background(), objects, testWorldID, []byte("strictness: block\n"))
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
			t.Fatalf("differing bootstrap error = %v", err)
		}
		store, err := bucketstore.Open(context.Background(), objects, bucketstore.Options{WorldID: testWorldID, RequirePolicy: true})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		document, err := store.Get(publishpolicy.DocumentPath, 0)
		if err != nil {
			t.Fatalf("get policy: %v", err)
		}
		if !bytes.Equal(document.Content, original) || document.Version != 1 {
			t.Errorf("policy = version %d %q, want original v1", document.Version, document.Content)
		}
	})

	t.Run("rejects invalid policy before initialization", func(t *testing.T) {
		objects := newMemory(t)
		err := bootstrap(context.Background(), objects, testWorldID, []byte("strictness: invalid\n"))
		if err == nil {
			t.Fatal("bootstrap accepted invalid policy")
		}
		listed, listErr := objects.List(context.Background(), "", "")
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(listed.Objects) != 0 {
			t.Errorf("objects created = %d, want 0", len(listed.Objects))
		}
	})

	t.Run("honors canceled context", func(t *testing.T) {
		objects := newMemory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := bootstrap(ctx, objects, testWorldID, []byte("strictness: block\n")); !errors.Is(err, context.Canceled) {
			t.Fatalf("bootstrap error = %v, want context canceled", err)
		}
	})
}

func newMemory(t *testing.T) *blob.Memory {
	t.Helper()
	objects, err := blob.NewMemory(maxObjectBytes)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	return objects
}
