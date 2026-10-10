package graphstore

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

const evidenceSource = "mark://world/src.md"

// observeSource records one complete document-view read of src.md.
func observeSource(t *testing.T, store *Store, version int, body string, meta ...map[string]string) {
	t.Helper()
	metadata := map[string]string{"version": fmt.Sprint(version), "etag": fmt.Sprintf("e%d", version)}
	for _, m := range meta {
		maps.Copy(metadata, m)
	}
	if !store.ObserveDocument(evidenceSource, graph.FetchResult{Status: protocol.StatusOK, Body: body, Metadata: metadata}) {
		t.Fatal("observation rejected")
	}
}

func outgoingEdges(t *testing.T, store *Store, to string) []NeighborhoodEdge {
	t.Helper()
	page, err := store.Neighborhood(evidenceSource, NeighborhoodOptions{Direction: NeighborhoodOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	for i := range page.Rows {
		if page.Rows[i].Node.URL == to {
			return page.Rows[i].Edges
		}
	}
	t.Fatalf("no row for %s in %+v", to, page.Rows)
	return nil
}

func locations(edges []NeighborhoodEdge) []string {
	var out []string
	for i := range edges {
		for _, o := range edges[i].Occurrences {
			out = append(out, fmt.Sprintf("%s:%s>%s", o.Rel, o.Anchor, o.Fragment))
		}
	}
	return out
}

// Two links to different sections of one target stay one edge with two
// located occurrences; a third, unlabeled, from another section is kept too.
func TestEvidenceKeepsFragmentsAndSourceSections(t *testing.T) {
	store := New()
	observeSource(t, store, 1, "# Src\n\n## Alpha\n\nSee [one](/t.md#one) and [two](/t.md#two).\n\n## Beta\n\n[](/t.md)\n")
	edges := outgoingEdges(t, store, "mark://world/t.md")
	if len(edges) != 1 || edges[0].Edge.Count != 3 || edges[0].Edge.Anchor != "alpha" {
		t.Fatalf("edges = %+v, want one aggregated edge of three occurrences", edges)
	}
	want := []string{":alpha>one", ":alpha>two", ":beta>"}
	if got := locations(edges); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("occurrences = %v, want %v", got, want)
	}
}

// A metadata relation keeps the fragment its value named.
func TestEvidenceKeepsRelationFragments(t *testing.T) {
	store := New()
	observeSource(t, store, 1, "# Src\n", map[string]string{"rel-depends-on": "/t.md#decision, /u.md"})
	if got := locations(outgoingEdges(t, store, "mark://world/t.md")); strings.Join(got, " ") != "depends-on:>decision" {
		t.Errorf("occurrences = %v", got)
	}
	if got := locations(outgoingEdges(t, store, "mark://world/u.md")); strings.Join(got, " ") != "depends-on:>" {
		t.Errorf("occurrences = %v", got)
	}
}

// A renamed heading at the next revision replaces the anchor; the old one
// does not linger beside it.
func TestEvidenceFollowsTheLatestRevision(t *testing.T) {
	store := New()
	observeSource(t, store, 1, "# Src\n\n## Old name\n\n[t](/t.md#x)\n")
	observeSource(t, store, 2, "# Src\n\n## New name\n\n[t](/t.md#y)\n")
	if got := locations(outgoingEdges(t, store, "mark://world/t.md")); strings.Join(got, " ") != ":new-name>y" {
		t.Errorf("occurrences = %v", got)
	}
}

// A source that moved on since it was read shows the aggregate edge alone:
// a crawl at a newer revision replaces the topology and hides stale locations.
func TestEvidenceHiddenWhenTheSourceMovedOn(t *testing.T) {
	store := New()
	observeSource(t, store, 1, "# Src\n\n## Alpha\n\n[t](/t.md#one)\n")
	if len(locations(outgoingEdges(t, store, "mark://world/t.md"))) != 1 {
		t.Fatal("evidence missing before the crawl")
	}
	g := graph.New()
	node := &graph.Node{URL: evidenceSource, Title: "Src", Status: protocol.StatusOK, Observation: graph.Observe(evidenceSource, map[string]string{"version": "2", "etag": "e2"})}
	node.Observation.Complete = true
	g.AddNode(node)
	g.AddEdgeInfo(graph.Edge{From: evidenceSource, To: "mark://world/t.md", Anchor: "alpha", Count: 1})
	store.Merge(g, nil)
	edges := outgoingEdges(t, store, "mark://world/t.md")
	if len(edges) != 1 || edges[0].Occurrences != nil {
		t.Errorf("stale occurrences shown after the source moved on: %+v", edges)
	}
	if _, kept := store.evidence[evidenceSource]; kept {
		t.Error("evidence for the superseded revision was retained")
	}
}

// Evidence survives a save and load, and a file without it loads as before.
func TestEvidenceRoundTripsThroughGraphJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	observeSource(t, store, 1, "# Src\n\n## Alpha\n\n[t](/t.md#one)\n")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := locations(outgoingEdges(t, loaded, "mark://world/t.md")); strings.Join(got, " ") != ":alpha>one" {
		t.Errorf("occurrences after load = %v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "evidence")
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	older, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if edges := outgoingEdges(t, older, "mark://world/t.md"); len(edges) != 1 || edges[0].Occurrences != nil {
		t.Errorf("a file saved by an older binary should load without occurrences: %+v", edges)
	}
}

// evidenceOwned is the heap a store holds after every one of docs sources
// was re-read at each of cycles revisions, each read with links located links.
func evidenceOwned(t *testing.T, cycles int) int64 {
	t.Helper()
	const docs, links = 64, 16
	return memtest.Owned(func() any {
		store := New()
		for cycle := 1; cycle <= cycles; cycle++ {
			for i := range docs {
				var b strings.Builder
				fmt.Fprintf(&b, "# Doc %d\n\n## Section %d\n\n", i, cycle)
				for j := range links {
					fmt.Fprintf(&b, "[link](/docs/%d.md#part-%d-%d)\n", (i+j)%docs, cycle, j)
				}
				url := fmt.Sprintf("mark://world/docs/%d.md", i)
				if !store.ObserveDocument(url, graph.FetchResult{Status: protocol.StatusOK, Body: b.String(), Metadata: map[string]string{"version": fmt.Sprint(cycle), "etag": fmt.Sprintf("e%d-%d", i, cycle)}}) {
					t.Fatal("observation rejected")
				}
			}
		}
		if len(store.evidence) != docs {
			t.Fatalf("evidence for %d sources, want %d", len(store.evidence), docs)
		}
		return store
	})
}

// Evidence tracks the current revision of each local source, so what the
// store holds does not grow with the revisions it has seen.
func TestEvidenceRetainsOnlyTheCurrentRevision(t *testing.T) {
	short := evidenceOwned(t, 5)
	long := evidenceOwned(t, 40)
	t.Logf("the store holds %d bytes after 5 revisions, %d after 40", short, long)
	if grew := long - short; grew > 64<<10 {
		t.Errorf("the store grew %d bytes over 35 more revisions of every source", grew)
	}
}
