package bucketstore

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// newQuotaStore opens an initialized in-memory store with MaxDocuments.
func newQuotaStore(t *testing.T, maxDocuments int) *Store {
	t.Helper()
	objects, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	if err := Initialize(context.Background(), objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	store, err := Open(context.Background(), objects, Options{WorldID: testWorldID, MaxDocuments: maxDocuments})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.commitInterval = 0
	return store
}

func TestMaxDocumentsRejectsNewPathBeyondQuota(t *testing.T) {
	store := newQuotaStore(t, 2)
	meta := map[string]string{"tags": "test"}
	if _, err := store.WriteVersion("/one.md", 0, []byte("# One\n"), meta); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := store.WriteVersion("/two.md", 0, []byte("# Two\n"), meta); err != nil {
		t.Fatalf("second write: %v", err)
	}
	_, err := store.WriteVersion("/three.md", 0, []byte("# Three\n"), meta)
	if !errors.Is(err, ErrDocumentQuota) {
		t.Fatalf("third write err = %v, want ErrDocumentQuota", err)
	}
	// Existing documents keep accepting updates and appends at quota.
	if _, err := store.WriteVersion("/one.md", 1, []byte("# One v2\n"), meta); err != nil {
		t.Fatalf("update at quota: %v", err)
	}
	if _, err := store.Append("/two.md", 1, []byte("\nmore\n"), nil); err != nil {
		t.Fatalf("append at quota: %v", err)
	}
}

func TestMaxDocumentsZeroIsUnlimited(t *testing.T) {
	store := newQuotaStore(t, 0)
	meta := map[string]string{"tags": "test"}
	for _, path := range []string{"/a.md", "/b.md", "/c.md"} {
		if _, err := store.WriteVersion(path, 0, []byte("# Doc\n"), meta); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}
