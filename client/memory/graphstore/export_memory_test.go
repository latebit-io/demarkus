package graphstore

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

// exportOwned is the heap a store holds after seeding from cycles of a
// /graph.md export in which one document changes per cycle.
func exportOwned(t *testing.T, cycles int) int64 {
	const docs = 64
	observed := time.Date(2026, 9, 29, 15, 58, 33, 0, time.UTC)
	title := strings.Repeat("agent inbox run ", 24)
	return memtest.Owned(func() any {
		store := New()
		revisions := make([]int, docs)
		for cycle := range cycles {
			revisions[cycle%docs]++
			nodes := make([]StoredNode, 0, docs)
			edges := make([]StoredEdge, 0, docs)
			for i := range docs {
				url := fmt.Sprintf("mark://w/agents/session-%d/inbox/01HZX%08d.md", i%7, i)
				rev := revisions[i] + 1
				nodes = append(nodes, StoredNode{URL: url, Title: fmt.Sprintf("%s%d", title, rev), Status: "ok", LinkCount: 1,
					Observation: graph.Observation{Source: url, View: graph.ViewDocument, Revision: rev, Etag: fmt.Sprintf("sha256-%064x", rev*docs+i), Complete: true, ObservedAt: observed}})
				edges = append(edges, StoredEdge{From: url, To: fmt.Sprintf("mark://w/hub-%d.md", i%7), Label: "hub", Anchor: "links", Count: 1})
			}
			parsedNodes, parsedEdges, err := ParseExportStrict(BuildExport(observed, nodes, edges))
			if err != nil {
				t.Fatal(err)
			}
			store.SeedFromExport(parsedNodes, parsedEdges)
		}
		if len(store.nodes) != docs {
			t.Fatalf("%d nodes, want %d", len(store.nodes), docs)
		}
		return store
	})
}

// A row kept from an earlier export must not keep that export's body.
func TestExportSeedDoesNotRetainExportBodies(t *testing.T) {
	short, long := exportOwned(t, 8), exportOwned(t, 56)
	t.Logf("the store holds %d bytes after 8 exports, %d after 56", short, long)
	// One export body is about 50 KB; 48 more pinned would be about 2.4 MB.
	if grew := long - short; grew > 256<<10 {
		t.Errorf("the store grew %d bytes over 48 more exports of the same documents", grew)
	}
}
