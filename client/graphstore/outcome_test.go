package graphstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
)

func TestCrawlPersistsPartialObservationsWithoutReplacingSource(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	const root = "mark://host/root.md"
	const oldTarget = "mark://host/old.md"
	old := graph.New()
	old.AddNode(&graph.Node{URL: root, Title: "Last good", Status: "ok"})
	old.AddEdgeInfo(graph.Edge{From: root, To: oldTarget, Count: 5})
	s.Merge(old, map[string]string{root: "old-etag"})
	var body strings.Builder
	body.WriteString("[old](/old.md) [new](/new.md)\n")
	for i := range 100 {
		fmt.Fprintf(&body, "[child](/child-%d.md)\n", i)
	}
	f := func(context.Context, links.Target) (graph.FetchResult, error) {
		return graph.FetchResult{Status: "ok", Body: body.String(), Metadata: map[string]string{"etag": "new-etag"}}, nil
	}
	g, err := s.CrawlAndPersist(t.Context(), root, f, CrawlOptions{MaxDepth: 0, MaxOutputBytes: 1024})
	if !errors.Is(err, graph.ErrIncomplete) || g.EdgeCount() == 0 {
		t.Fatalf("partial crawl = %v", err)
	}
	restored, err := Load(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if n := restored.GetNode(root); n == nil || n.Title != "Last good" || n.Etag != "old-etag" {
		t.Fatalf("partial source replaced last good: %+v", n)
	}
	if edges := restored.BacklinksEnriched(oldTarget); len(edges) != 1 || edges[0].Count != 5 {
		t.Fatalf("partial count replaced last good: %+v", edges)
	}
	if len(restored.Backlinks("mark://host/new.md")) != 1 {
		t.Fatal("valid new boundary edge was lost")
	}
}

func TestFailedStatusDoesNotEraseSeed(t *testing.T) {
	for _, status := range []string{"unauthorized", "server-error", "rate-limited", "partial"} {
		t.Run(status, func(t *testing.T) {
			s := New()
			s.ReplaceSeed("host", []StoredNode{{URL: "mark://host/root.md", Status: "ok", Title: "Seed"}}, []StoredEdge{{From: "mark://host/root.md", To: "mark://host/child.md"}})
			g := graph.New()
			g.AddNode(&graph.Node{URL: "mark://host/root.md", Status: status})
			s.Merge(g, nil)
			if s.GetNode("mark://host/root.md").Title != "Seed" || s.EdgeCount() != 1 {
				t.Fatal("failed source erased seed")
			}
		})
	}
}

func TestCrawlExternalRootWithoutFetcher(t *testing.T) {
	s := New()
	const root = "https://example.com/"
	g, err := s.CrawlAndPersist(t.Context(), root, nil, CrawlOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Outcome.Complete || g.Outcome.Fetches != 0 || s.GetNode(root).Status != "external" {
		t.Fatalf("external crawl = %+v", g.Outcome)
	}
}
