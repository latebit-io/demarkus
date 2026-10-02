package graphstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

// agentSource is a production-shaped row: an agent inbox path, a title and
// etag new at every version, and edges to other agent documents.
func agentSource(world string, i, version, links int) WorldSource {
	src := WorldSource{
		Path:    fmt.Sprintf("/agents/session-%d/inbox/01HZX%08d.md", i%7, i),
		Version: version,
		Title:   fmt.Sprintf("Run 2026-09-29T15:58:33.%09dZ", version*1_000_000+i),
		Etag:    fmt.Sprintf("sha256-%064x", version*1_000_000+i),
	}
	for j := range links {
		src.Edges = append(src.Edges, WorldEdge{To: fmt.Sprintf("mark://%s/agents/session-%d/inbox/01HZX%08d.md", world, j%7, i*links+j), Count: 1})
	}
	return src
}

// seedOwned is the heap a store holds after cycles of edits, creates and
// archives in two worlds, each seeded incrementally over its pins, and the
// live edges it shows.
func seedOwned(t *testing.T, cycles int) (owned int64, edges int) {
	const live, edits, churn, links = 64, 4, 16, 16
	ctx, hub, worlds := context.Background(), newTestHub(), []string{"alpha", "beta"}
	owned = memtest.Owned(func() any {
		store, rows := New(), map[string][]WorldSource{}
		for _, world := range worlds {
			for i := range live {
				rows[world] = append(rows[world], agentSource(world, i, 1, links))
			}
		}
		for cycle := range cycles {
			for _, world := range worlds {
				sources := rows[world]
				for i := range edits {
					k := (cycle*edits + i) % (live - churn)
					sources[k] = agentSource(world, k, sources[k].Version+1, links)
				}
				// The last rows are archived and new documents take their place.
				for i := range churn {
					sources[live-churn+i] = agentSource(world, live+cycle*churn+i, 1, links)
				}
				hub.publishCheckpoint(t, WorldManifest{World: world, Cursor: protocol.Cursor{Epoch: "e1", Seq: uint64(cycle + 1)}, Complete: true, PrefixLength: 1}, sources)
				seedWorld(ctx, store, hub, world)
			}
		}
		if len(store.nodes) != len(worlds)*live || len(store.worldPins) != len(worlds) {
			t.Fatalf("%d nodes and pins for %d worlds, want %d and %d", len(store.nodes), len(store.worldPins), len(worlds)*live, len(worlds))
		}
		return store
	})
	return owned, len(worlds) * live * links
}

// The gateway's store tracks each world's live checkpoint rows, so what it
// holds does not grow with the churn it has seen.
func TestSeedRetainsOnlyTheLiveCheckpointRows(t *testing.T) {
	short, edges := seedOwned(t, 50)
	long, _ := seedOwned(t, 200)
	t.Logf("the store holds %d bytes after 50 cycles, %d after 200", short, long)
	// About 700 bytes per live edge across the seed records and indexes.
	// Near zero means something else holds the store.
	if floor, limit := int64(edges*256), int64(edges<<10); long < floor || long > limit {
		t.Errorf("the store holds %d bytes, want %d to %d", long, floor, limit)
	}
	// Layout varies by about 60 KB; each churned source remembered is about
	// 100 bytes, 480 KB here.
	if grew := long - short; grew > 128<<10 {
		t.Errorf("the store grew %d bytes over 150 more cycles of churn", grew)
	}
}
