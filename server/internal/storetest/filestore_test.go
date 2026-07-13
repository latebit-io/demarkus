package storetest

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// TestFileStoreConformance runs the conformance suite against the file
// store, pinning the reference behavior every other backend must match.
func TestFileStoreConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) handler.DocumentStore {
		return store.New(t.TempDir())
	})
}

// TestFileStoreLookupConformance runs the LOOKUP conformance suite against
// the file backend's pairing: file store plus in-memory catalog, kept in
// sync by the handler-mirroring helpers.
func TestFileStoreLookupConformance(t *testing.T) {
	RunLookupConformance(t, func(t *testing.T) LookupBackend {
		return LookupBackend{Store: store.New(t.TempDir()), Catalog: catalog.New()}
	})
}
