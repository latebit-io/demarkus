package filestore

import (
	"context"
	"testing"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// ReadCurrent takes no store lock: a commit in one world may read another
// world's policy while both hold their own write locks.
func TestReadCurrentTakesNoStoreLock(t *testing.T) {
	store := New(protocolstore.New(t.TempDir()), catalog.New())
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/p.md", Content: []byte("# P\n")}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		doc, err := store.ReadCurrent(context.Background(), "/p.md")
		if err == nil && string(doc.Content) != "# P\n" {
			t.Errorf("content = %q", doc.Content)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadCurrent: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadCurrent waited on the store lock")
	}
	if _, err := store.ReadCurrent(context.Background(), "/missing.md"); err == nil {
		t.Fatal("missing document read as present")
	}
}
