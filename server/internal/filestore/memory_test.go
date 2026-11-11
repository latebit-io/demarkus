package filestore

import (
	"runtime"
	"testing"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	protocolstore "github.com/latebit-io/demarkus/server/store"
)

// Versions live on disk, so an agent-shaped publish-and-prune loop must leave
// heap tracking the live documents, not the writes. The WATCH ring is small
// so warmup fills it: the journal tail and hub must evict, not accumulate.
func TestPublishPruneHeapTracksLiveData(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		checkPublishPruneHeap(t, New(protocolstore.New(t.TempDir()), catalog.New()))
	})
	t.Run("watch", func(t *testing.T) {
		store, _ := openWatched(t, t.TempDir(), 8)
		t.Cleanup(func() { closeStore(t, store) })
		checkPublishPruneHeap(t, store)
	})
}

func checkPublishPruneHeap(t *testing.T, store *Store) {
	t.Helper()
	ctx := t.Context()
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

// A rebuild after an outside write swaps the catalog whole: repeated direct
// publishes must leave the heap at one index and catalog, not one per rebuild.
func TestOutsideWriteRebuildHeapTracksLiveData(t *testing.T) {
	dir := t.TempDir()
	tool := protocolstore.New(dir)
	publish := func(n int) {
		if _, err := tool.Write("/graph.md", memtest.AgentGraphBody(n), nil); err != nil {
			t.Fatalf("outside publish %d: %v", n, err)
		}
		if err := tool.MarkChanged(); err != nil {
			t.Fatal(err)
		}
	}
	publish(0)
	store, err := OpenDir(dir, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeStore(t, store) })
	read := func() {
		view, err := store.OpenReadView(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
	}
	const warmup, cycles = 5, 40
	for n := 1; n <= warmup; n++ {
		publish(n)
		read()
	}
	growth := memtest.Retained(func() {
		for n := warmup + 1; n <= warmup+cycles; n++ {
			publish(n)
			read()
		}
	})
	runtime.KeepAlive(store)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes", growth)
	if limit := 2 * bodyBytes; growth > limit {
		t.Errorf("heap grew %d bytes over %d outside publishes of a %d-byte body, want under %d", growth, cycles, bodyBytes, limit)
	}
}
