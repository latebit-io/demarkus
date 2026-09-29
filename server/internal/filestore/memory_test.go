package filestore

import (
	"runtime"
	"testing"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/memtest"
)

// Versions live on disk, so an agent-shaped publish-and-prune loop must leave
// heap tracking the live documents, not the writes.
func TestPublishPruneHeapTracksLiveData(t *testing.T) {
	ctx := t.Context()
	store := New(protocolstore.New(t.TempDir()), catalog.New(), nil)
	meta := map[string]string{"retention": "20", "agent": "federation"}
	version := 0
	publish := func(n int) {
		document, err := store.Publish(ctx, backend.WriteRequest{Path: "/graph.md", ExpectedVersion: version, Content: memtest.AgentGraphBody(n), Metadata: meta})
		if err != nil {
			t.Fatalf("publish %d: %v", n, err)
		}
		version = document.Version
	}
	// Past retention, so every measured write also prunes.
	const warmup, cycles = 25, 60
	for n := range warmup {
		publish(n)
	}
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			publish(n)
		}
	})
	runtime.KeepAlive(store)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes", growth)
	if limit := 2 * bodyBytes; growth > limit {
		t.Errorf("heap grew %d bytes over %d publish-and-prune cycles of a %d-byte body, want under %d", growth, cycles, bodyBytes, limit)
	}
}
