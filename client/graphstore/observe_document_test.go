package graphstore

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
)

func TestObserveDocumentReplacesMetadataRelations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	docURL := "mark://world/source.md"
	body := "# Source\n\n[body](/body.md)\n"
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: body,
		Metadata: map[string]string{"version": "1", "etag": "one", "rel-depends-on": "/old.md"},
	})
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: body,
		Metadata: map[string]string{"version": "2", "etag": "two", "rel-related": "/new.md"},
	})

	page, err := store.Neighborhood(docURL, NeighborhoodOptions{Direction: NeighborhoodOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if got := neighborhoodURLs(page); !slices.Equal(got, []string{"mark://world/body.md", "mark://world/new.md"}) {
		t.Fatalf("outgoing URLs = %v", got)
	}
	if page.Rows[0].Edges[0].Edge.Rel != "" || page.Rows[1].Edges[0].Edge.Rel != "related" {
		t.Fatalf("outgoing evidence = %+v", page.Rows)
	}
	for i := range page.Rows {
		row := &page.Rows[i]
		if source := row.Edges[0].Source; source.URL != docURL || source.Title != "Source" || source.LinkCount != 1 || source.Observation.Revision != 2 || source.Etag != "two" {
			t.Fatalf("source evidence = %+v", source)
		}
	}
	if node := store.GetNode(docURL); node == nil || node.Title != "Source" || node.LinkCount != 1 || node.Observation.Revision != 2 {
		t.Fatalf("observed node = %+v", node)
	}
	if got := store.Backlinks("mark://world/old.md"); len(got) != 0 {
		t.Fatalf("removed relation remains in legacy backlinks: %v", got)
	}
	if got := store.Backlinks("mark://world/new.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("legacy backlinks = %v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ObserveDocument persisted internally: %v", err)
	}
}

func TestObserveDocumentIncompleteKeepsLastGoodAdjacency(t *testing.T) {
	store := New()
	docURL := "mark://world/source.md"
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: "# Source\n\n[target](/target.md)",
		Metadata: map[string]string{"version": "1", "etag": "one"},
	})
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "error", Metadata: map[string]string{"version": "2", "etag": "two"},
	})
	if got := store.Backlinks("mark://world/target.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("incomplete observation replaced last-good adjacency: %v", got)
	}
	if node := store.GetNode(docURL); node == nil || node.Observation.Revision != 1 || node.Etag != "one" || node.Observation.Problem != "incomplete" {
		t.Fatalf("incomplete observation replaced node: %+v", node)
	}
}

func TestObserveDocumentSkipsGeneratedGraphArtifacts(t *testing.T) {
	for _, docURL := range []string{
		"mark://world/graph.md",
		"mark://world/graph.md/v3",
		"mark://world/graph/manifest.md",
		"mark://world/graph/manifest.md/v2",
		"mark://world/graph/shards/a/edges-000.md",
		"mark://world/graph/shards/a/edges-000.md/v1",
	} {
		t.Run(docURL, func(t *testing.T) {
			store := New()
			changed := store.ObserveDocument(docURL, graph.FetchResult{
				Status: "ok", Body: "# Generated\n\n[synthetic](/target.md)",
				Metadata: map[string]string{"version": "1"},
			})
			if changed || store.NodeCount() != 0 || store.EdgeCount() != 0 {
				t.Fatalf("generated artifact entered graph: changed=%t nodes=%d edges=%d", changed, store.NodeCount(), store.EdgeCount())
			}
		})
	}
}

func TestObserveDocumentConfirmedAbsenceClearsAdjacency(t *testing.T) {
	for _, status := range []string{"not-found", "archived"} {
		t.Run(status, func(t *testing.T) {
			store := New()
			docURL := "mark://world/source.md"
			store.ObserveDocument(docURL, graph.FetchResult{
				Status: "ok", Body: "# Source\n\n[target](/target.md)",
				Metadata: map[string]string{"version": "1", "etag": "one"},
			})
			store.ObserveDocument(docURL, graph.FetchResult{Status: status})
			if got := store.Backlinks("mark://world/target.md"); len(got) != 0 {
				t.Fatalf("confirmed absence retained adjacency: %v", got)
			}
			if node := store.GetNode(docURL); node == nil || node.Status != status {
				t.Fatalf("confirmed absence node = %+v", node)
			}
		})
	}
}
