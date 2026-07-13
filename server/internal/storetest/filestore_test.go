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
	})
}
