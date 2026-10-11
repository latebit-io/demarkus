package storetest

import (
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// TestFileStoreConformance runs the suite against the filestore backend that
// production serves; tampering reaches the raw store under it.
func TestFileStoreConformance(t *testing.T) {
	var mu sync.Mutex
	documents := map[handler.DocumentStore]*store.Store{}
	RunConformance(t, func(t *testing.T) handler.DocumentStore {
		raw := store.New(t.TempDir())
		wrapped := filestore.New(raw, catalog.New())
		mu.Lock()
		documents[wrapped] = raw
		mu.Unlock()
		return wrapped
	}, func(t testing.TB, s handler.DocumentStore, path string, version int, stored []byte) {
		mu.Lock()
		raw := documents[s]
		mu.Unlock()
		FileTamper(t, raw, path, version, stored)
	})
}

// TestFileStoreLookupConformance runs the LOOKUP conformance suite against
// the file backend's pairing: file store plus in-memory catalog, kept in
// sync by the handler-mirroring helpers.
func TestFileStoreLookupConformance(t *testing.T) {
	RunLookupConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreLookupHandlerConformance pins the LOOKUP wire contract for
// the match key on the file backend.
func TestFileStoreLookupHandlerConformance(t *testing.T) {
	RunLookupHandlerConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreHandlerConformance pins backend dependent wire behavior.
func TestFileStoreHandlerConformance(t *testing.T) {
	RunHandlerConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreDifferentialSelf runs the differential harness with the file
// store on both sides. It proves the harness itself is deterministic and
// backend-neutral: a self-diff failure is a harness bug, not a store bug.
func TestFileStoreDifferentialSelf(t *testing.T) {
	factory := func(t *testing.T) LookupBackend { return FileBackend(t) }
	RunDifferential(t, factory, factory, DifferentialConfig{Seeds: []int64{1, 2}, Ops: 80})
}

// TestFileStoreHandlerDifferentialSelf proves the handler-level harness is
// deterministic with the file store on both sides.
func TestFileStoreHandlerDifferentialSelf(t *testing.T) {
	factory := func(t *testing.T) LookupBackend { return FileBackend(t) }
	RunHandlerDifferential(t, factory, factory, DifferentialConfig{Seeds: []int64{1, 2}, Ops: 80})
}

func BenchmarkFileHandler(b *testing.B) {
	RunHandlerBenchmarks(b, FileBackend)
}

// TestFileStoreRejectionConformance: with the policy decorator above it the
// file backend refuses exactly as the bucket backend does. It has no quota.
func TestFileStoreRejectionConformance(t *testing.T) {
	RunRejectionConformance(t, RejectionFactories{
		Policy: func(t *testing.T) LookupBackend {
			b := FileBackend(t)
			policy := []byte("# Write Policy\n\nCurated.\n\nstrictness: block\nrequire_tags: domain\n")
			meta := map[string]string{"tags": "category:governance", "type": "Policy"}
			if _, err := b.direct().WriteVersion(publishpolicy.DocumentPath, 0, policy, meta); err != nil {
				t.Fatalf("seed policy: %v", err)
			}
			b.Store = writepolicy.Enforce(b.Store, writepolicy.Options{Require: true})
			return b
		},
	})
}
