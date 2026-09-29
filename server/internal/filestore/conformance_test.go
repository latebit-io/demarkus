package filestore_test

import (
	"context"
	"log/slog"
	"testing"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// fileWindow keeps the journal ring small so the window cases stay cheap,
// yet above what the concurrency case keeps in flight.
const fileWindow = 64

// fileSite is one content root. Tamper writes through a store with no
// journal, as demarkus-publish or an operator would while the server is down.
type fileSite struct{ root string }

// open opens the root with the hash index built; ring 0 is a store with no
// journal, which is what an out-of-band writer looks like.
func (s *fileSite) open(t *testing.T, ring int) *filestore.Store {
	t.Helper()
	documents, err := protocolstore.Open(s.root)
	if err != nil {
		t.Fatalf("open documents: %v", err)
	}
	if err := documents.BuildHashIndex(); err != nil {
		t.Fatalf("hash index: %v", err)
	}
	store, err := filestore.Open(documents, catalog.New(), filestore.Options{ChangeRing: ring, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func (s *fileSite) Open(t *testing.T) storetest.ChangeBackend {
	store := s.open(t, fileWindow)
	return storetest.ChangeBackend{Store: store, Close: store.Close}
}

func (s *fileSite) Tamper(t *testing.T, path string) {
	t.Helper()
	if _, err := s.open(t, 0).Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# behind\n")}); err != nil {
		t.Fatalf("tamper %s: %v", path, err)
	}
}

func (s *fileSite) Window() int { return fileWindow }

func TestChangeConformance(t *testing.T) {
	storetest.RunChangeConformance(t, func(t *testing.T) storetest.ChangeSite {
		return &fileSite{root: t.TempDir()}
	})
}
