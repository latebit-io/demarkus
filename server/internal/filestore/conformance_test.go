package filestore_test

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// fileWindow keeps the journal ring small so the window cases stay cheap,
// yet above what the concurrency case keeps in flight.
const fileWindow = 64

// fileSite is one content root. Tamper writes through a store with no
// journal, as demarkus-publish or an operator would while the server is down.
type fileSite struct{ root string }

func (s *fileSite) Open(t *testing.T) storetest.ChangeBackend {
	store, _ := filestore.OpenWatched(t, s.root, fileWindow)
	return storetest.ChangeBackend{Store: store, Close: store.Close}
}

// Tamper writes through a store with no journal (ring 0), which is what an
// out-of-band writer looks like.
func (s *fileSite) Tamper(t *testing.T, path string) {
	t.Helper()
	store, _ := filestore.OpenWatched(t, s.root, 0)
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# behind\n")}); err != nil {
		t.Fatalf("tamper %s: %v", path, err)
	}
}

func (s *fileSite) Window() int { return fileWindow }

func TestChangeConformance(t *testing.T) {
	storetest.RunChangeConformance(t, func(t *testing.T) storetest.ChangeSite {
		return &fileSite{root: t.TempDir()}
	})
}
