package storetest

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// TestFileStoreConformance runs the conformance suite against the file
// store, pinning the reference behavior every other backend must match.
func TestFileStoreConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) handler.DocumentStore {
		return store.New(t.TempDir())
	}, FileTamper)
}

// TestFileStoreLookupConformance runs the LOOKUP conformance suite against
// the file backend's pairing: file store plus in-memory catalog, kept in
// sync by the handler-mirroring helpers.
func TestFileStoreLookupConformance(t *testing.T) {
	RunLookupConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
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
