package graphstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
)

func TestLoadEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if s.NodeCount() != 0 {
		t.Errorf("NodeCount = %d, want 0", s.NodeCount())
	}
	if s.EdgeCount() != 0 {
		t.Errorf("EdgeCount = %d, want 0", s.EdgeCount())
	}
}

func TestLoadEmptyPath(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("Load empty path: expected error")
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/index.md", Title: "Home", Status: "ok", LinkCount: 2})
	g.AddNode(&graph.Node{URL: "mark://a:6309/about.md", Title: "About", Status: "ok", LinkCount: 0})
	g.AddEdge("mark://a:6309/index.md", "mark://a:6309/about.md")

	etags := map[string]string{
		"mark://a:6309/index.md": "etag-1",
		"mark://a:6309/about.md": "etag-2",
	}
	count := s.Merge(g, etags)
	if count != 2 {
		t.Errorf("Merge count = %d, want 2", count)
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if s2.NodeCount() != 2 {
		t.Errorf("NodeCount = %d, want 2", s2.NodeCount())
	}
	if s2.EdgeCount() != 1 {
		t.Errorf("EdgeCount = %d, want 1", s2.EdgeCount())
	}

	n := s2.GetNode("mark://a:6309/index.md")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	if n.Title != "Home" {
		t.Errorf("Title = %q, want %q", n.Title, "Home")
	}
	if n.Etag != "etag-1" {
		t.Errorf("Etag = %q, want %q", n.Etag, "etag-1")
	}
	if n.CrawledAt.IsZero() {
		t.Error("CrawledAt is zero")
	}
}

func TestMergeUpdatesNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g1 := graph.New()
	g1.AddNode(&graph.Node{URL: "mark://a:6309/doc.md", Title: "Old", Status: "ok"})
	s.Merge(g1, nil)

	g2 := graph.New()
	g2.AddNode(&graph.Node{URL: "mark://a:6309/doc.md", Title: "New", Status: "ok"})
	s.Merge(g2, nil)

	if s.NodeCount() != 1 {
		t.Errorf("NodeCount = %d, want 1", s.NodeCount())
	}
	n := s.GetNode("mark://a:6309/doc.md")
	if n.Title != "New" {
		t.Errorf("Title = %q, want %q", n.Title, "New")
	}
}

func TestMergeAddsEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g1 := graph.New()
	g1.AddNode(&graph.Node{URL: "mark://a:6309/a.md"})
	g1.AddNode(&graph.Node{URL: "mark://a:6309/b.md"})
	g1.AddEdge("mark://a:6309/a.md", "mark://a:6309/b.md")
	s.Merge(g1, nil)

	g2 := graph.New()
	g2.AddNode(&graph.Node{URL: "mark://a:6309/a.md"})
	g2.AddNode(&graph.Node{URL: "mark://a:6309/b.md"})
	g2.AddNode(&graph.Node{URL: "mark://a:6309/c.md"})
	g2.AddEdge("mark://a:6309/a.md", "mark://a:6309/b.md") // duplicate
	g2.AddEdge("mark://a:6309/a.md", "mark://a:6309/c.md") // new
	s.Merge(g2, nil)

	if s.EdgeCount() != 2 {
		t.Errorf("EdgeCount = %d, want 2", s.EdgeCount())
	}
}

func TestMergeEnrichedEdgeIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md", Label: "B doc", Anchor: "intro", Count: 3})
	s.Merge(g, nil)
	s.Merge(g, nil) // re-merging the same crawl must not inflate counts

	edges := s.edges
	if len(edges) != 1 {
		t.Fatalf("EdgeCount = %d, want 1", len(edges))
	}
	e := edges[0]
	if e.Label != "B doc" || e.Anchor != "intro" || e.Count != 3 {
		t.Errorf("edge = %+v, want Label=B doc Anchor=intro Count=3", e)
	}
}

func TestMergeReplacesRefreshedSourceEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g1 := graph.New()
	g1.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Status: "ok", LinkCount: 1})
	g1.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md"})
	g1.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/old.md", Rel: "supersedes"})
	s.Merge(g1, nil)

	// The doc dropped the link to old.md and its rel- key; a refreshed crawl
	// must replace the whole outgoing set, not just upsert.
	g2 := graph.New()
	g2.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Status: "ok", LinkCount: 1})
	g2.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md"})
	s.Merge(g2, nil)

	if s.EdgeCount() != 1 {
		t.Fatalf("EdgeCount = %d, want 1: %+v", s.EdgeCount(), s.edges)
	}
	if bl := s.Backlinks("mark://a:6309/old.md"); len(bl) != 0 {
		t.Errorf("stale backlink survived refresh: %v", bl)
	}
}

func TestMergeKeepsEdgesOfUnfetchedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g1 := graph.New()
	g1.AddNode(&graph.Node{URL: "mark://a:6309/x.md", Status: "ok", LinkCount: 1})
	g1.AddEdgeInfo(graph.Edge{From: "mark://a:6309/x.md", To: "mark://a:6309/y.md"})
	s.Merge(g1, nil)

	// A later crawl that only saw x.md as an error must not drop the edges
	// recorded when it was last read successfully.
	g2 := graph.New()
	g2.AddNode(&graph.Node{URL: "mark://a:6309/x.md", Status: "error"})
	s.Merge(g2, nil)

	if s.EdgeCount() != 1 {
		t.Fatalf("EdgeCount = %d, want 1 (error-status source must keep edges)", s.EdgeCount())
	}
}

func TestMergeKeepsDistinctRels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md"})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md", Rel: "supersedes"})
	s.Merge(g, nil)

	if s.EdgeCount() != 2 {
		t.Errorf("EdgeCount = %d, want 2", s.EdgeCount())
	}
}

func TestLoadLegacyEdgesDefaultsCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	legacy := `{"version":1,"nodes":[{"url":"mark://a:6309/a.md","title":"A","status":"ok","link_count":1,"crawled_at":"2026-01-01T00:00:00Z"}],"edges":[{"from":"mark://a:6309/a.md","to":"mark://a:6309/b.md"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	edges := s.edges
	if len(edges) != 1 {
		t.Fatalf("EdgeCount = %d, want 1", len(edges))
	}
	e := edges[0]
	if e.Count != 1 || e.Rel != "" || e.Label != "" || e.Anchor != "" {
		t.Errorf("legacy edge = %+v, want Count=1 and empty enrichment", e)
	}
}

func TestLoadRejectsSchemaVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"nodes":[],"edges":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("Load = %v, want unsupported schema version error", err)
	}
}

func TestBacklinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "A"})
	g.AddNode(&graph.Node{URL: "mark://a:6309/b.md", Title: "B"})
	g.AddNode(&graph.Node{URL: "mark://a:6309/c.md", Title: "C"})
	g.AddEdge("mark://a:6309/a.md", "mark://a:6309/c.md")
	g.AddEdge("mark://a:6309/b.md", "mark://a:6309/c.md")
	s.Merge(g, nil)

	backlinks := s.Backlinks("mark://a:6309/c.md")
	if len(backlinks) != 2 {
		t.Fatalf("len(backlinks) = %d, want 2", len(backlinks))
	}
	if backlinks[0] != "mark://a:6309/a.md" {
		t.Errorf("backlinks[0] = %q, want %q", backlinks[0], "mark://a:6309/a.md")
	}
	if backlinks[1] != "mark://a:6309/b.md" {
		t.Errorf("backlinks[1] = %q, want %q", backlinks[1], "mark://a:6309/b.md")
	}
}

func TestBacklinksNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	backlinks := s.Backlinks("mark://a:6309/unknown.md")
	if len(backlinks) != 0 {
		t.Errorf("len(backlinks) = %d, want 0", len(backlinks))
	}
}

func TestToGraph(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "A", Status: "ok", LinkCount: 1})
	g.AddNode(&graph.Node{URL: "mark://a:6309/b.md", Title: "B", Status: "ok"})
	g.AddEdge("mark://a:6309/a.md", "mark://a:6309/b.md")
	s.Merge(g, nil)

	g2 := s.ToGraph()
	if g2.NodeCount() != 2 {
		t.Errorf("NodeCount = %d, want 2", g2.NodeCount())
	}
	if g2.EdgeCount() != 1 {
		t.Errorf("EdgeCount = %d, want 1", g2.EdgeCount())
	}

	n := g2.GetNode("mark://a:6309/a.md")
	if n == nil {
		t.Fatal("GetNode returned nil")
	}
	if n.Title != "A" {
		t.Errorf("Title = %q, want %q", n.Title, "A")
	}
	if n.Depth != 0 {
		t.Errorf("Depth = %d, want 0", n.Depth)
	}
}

func TestSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md"})
	s.Merge(g, nil)

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// .tmp file should not remain after successful save
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file still exists after save")
	}

	// main file should exist
	if _, err := os.Stat(path); err != nil {
		t.Errorf("graph.json not found: %v", err)
	}
}

func TestBacklinksEnriched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "A", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://a:6309/b.md", Title: "B", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://a:6309/c.md", Title: "C", Status: "ok"})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/c.md", Label: "see C", Anchor: "notes", Count: 2})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/c.md", Rel: "supersedes"})
	g.AddEdge("mark://a:6309/b.md", "mark://a:6309/c.md")
	s.Merge(g, nil)

	entries := s.BacklinksEnriched("mark://a:6309/c.md")
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	// Sorted by URL then Rel: a.md plain, a.md supersedes, b.md.
	if entries[0].URL != "mark://a:6309/a.md" || entries[0].Rel != "" {
		t.Errorf("entries[0] = %+v, want a.md plain", entries[0])
	}
	if entries[0].Title != "A" {
		t.Errorf("entries[0].Title = %q, want A", entries[0].Title)
	}
	if entries[0].Status != "ok" {
		t.Errorf("entries[0].Status = %q, want ok", entries[0].Status)
	}
	if entries[0].Label != "see C" || entries[0].Anchor != "notes" || entries[0].Count != 2 {
		t.Errorf("entries[0] = %+v, want Label=see C Anchor=notes Count=2", entries[0])
	}
	if entries[1].URL != "mark://a:6309/a.md" || entries[1].Rel != "supersedes" {
		t.Errorf("entries[1] = %+v, want a.md supersedes", entries[1])
	}
	if entries[2].URL != "mark://a:6309/b.md" {
		t.Errorf("entries[2].URL = %q, want b.md", entries[2].URL)
	}
}

func TestBacklinksDedupesMultiEdgeSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	g := graph.New()
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/c.md"})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/c.md", Rel: "supersedes"})
	s.Merge(g, nil)

	backlinks := s.Backlinks("mark://a:6309/c.md")
	if len(backlinks) != 1 {
		t.Fatalf("len = %d, want 1 (source deduped across its edges)", len(backlinks))
	}
}

func TestBacklinksEnrichedNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entries := s.BacklinksEnriched("mark://a:6309/unknown.md")
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestAllNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(s.AllNodes()) != 0 {
		t.Errorf("AllNodes on empty = %d, want 0", len(s.AllNodes()))
	}

	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "A", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://a:6309/b.md", Title: "B", Status: "ok"})
	s.Merge(g, nil)

	nodes := s.AllNodes()
	if len(nodes) != 2 {
		t.Fatalf("AllNodes = %d, want 2", len(nodes))
	}

	urls := map[string]bool{}
	for _, n := range nodes {
		urls[n.URL] = true
	}
	if !urls["mark://a:6309/a.md"] || !urls["mark://a:6309/b.md"] {
		t.Errorf("missing expected URLs: %v", urls)
	}
}

func TestCrawlAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Mock fetcher: index links to about, about has no links.
	pages := map[string]struct{ body, etag string }{
		"host:6309/index.md": {body: "# Home\n[About](/about.md)\n", etag: "etag-1"},
		"host:6309/about.md": {body: "# About\n", etag: "etag-2"},
	}

	fetchFunc := func(host, path string) (graph.FetchResult, error) {
		key := host + path
		p, ok := pages[key]
		if !ok {
			return graph.FetchResult{Status: "not-found"}, nil
		}
		return graph.FetchResult{Status: "ok", Body: p.body, Metadata: map[string]string{"etag": p.etag}}, nil
	}

	parseURL := func(raw string) (string, string, error) {
		if len(raw) > 7 && raw[:7] == "mark://" {
			rest := raw[7:]
			for i := range len(rest) {
				if rest[i] == '/' {
					return rest[:i], rest[i:], nil
				}
			}
		}
		return "", "", fmt.Errorf("invalid URL: %s", raw)
	}

	var nodeCount atomic.Int32
	g, err := s.CrawlAndPersist(context.Background(), "mark://host:6309/index.md", fetchFunc, parseURL, CrawlOptions{
		MaxDepth: 2,
		MaxNodes: 100,
		OnNode: func(_ *graph.Node) {
			nodeCount.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("CrawlAndPersist: %v", err)
	}

	if g.NodeCount() < 2 {
		t.Errorf("graph NodeCount = %d, want >= 2", g.NodeCount())
	}
	if nodeCount.Load() < 2 {
		t.Errorf("OnNode called %d times, want >= 2", nodeCount.Load())
	}

	// Verify persistence
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after persist: %v", err)
	}
	if s2.NodeCount() < 2 {
		t.Errorf("stored NodeCount = %d, want >= 2", s2.NodeCount())
	}

	n := s2.GetNode("mark://host:6309/index.md")
	if n == nil {
		t.Fatal("stored node not found")
	}
	if n.Etag != "etag-1" {
		t.Errorf("Etag = %q, want %q", n.Etag, "etag-1")
	}
}

func TestCrawlAndPersist_NilStore(t *testing.T) {
	var s *Store

	fetchFunc := func(_, _ string) (graph.FetchResult, error) {
		return graph.FetchResult{Status: "ok", Body: "# Doc\n", Metadata: map[string]string{"etag": "etag-1"}}, nil
	}
	parseURL := func(_ string) (string, string, error) {
		return "host:6309", "/doc.md", nil
	}

	g, err := s.CrawlAndPersist(context.Background(), "mark://host:6309/doc.md", fetchFunc, parseURL, CrawlOptions{
		MaxDepth: 1,
	})
	if err != nil {
		t.Fatalf("CrawlAndPersist on nil store: %v", err)
	}
	if g.NodeCount() != 1 {
		t.Errorf("NodeCount = %d, want 1", g.NodeCount())
	}
}

func TestSeedFromExportEmptyStore(t *testing.T) {
	s := New()

	nodes := []StoredNode{
		{URL: "mark://a:6309/a.md", Title: "A", Status: "ok", LinkCount: 1},
		{URL: "mark://a:6309/b.md", Title: "B", Status: "ok"},
	}
	edges := []StoredEdge{
		{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md", Rel: "", Label: "B", Anchor: "intro", Count: 2},
	}

	added := s.SeedFromExport(nodes, edges)
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
	if s.EdgeCount() != 1 {
		t.Errorf("EdgeCount = %d, want 1", s.EdgeCount())
	}
	if bl := s.Backlinks("mark://a:6309/b.md"); len(bl) != 1 || bl[0] != "mark://a:6309/a.md" {
		t.Errorf("Backlinks = %v, want [mark://a:6309/a.md]", bl)
	}
	n := s.GetNode("mark://a:6309/a.md")
	if n == nil {
		t.Fatal("seeded node missing")
	}
	if !n.CrawledAt.IsZero() {
		t.Error("seeded node must have zero CrawledAt")
	}
}

func TestSeedFromExportLocalWins(t *testing.T) {
	s := New()

	// Local crawl: a.md links to b.md.
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "Local A", Status: "ok", LinkCount: 1})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md"})
	s.Merge(g, nil)

	// Seed claims a.md has a different title and links only to c.md.
	nodes := []StoredNode{{URL: "mark://a:6309/a.md", Title: "Hub A", Status: "ok", LinkCount: 1}}
	edges := []StoredEdge{{From: "mark://a:6309/a.md", To: "mark://a:6309/c.md", Count: 1}}
	added := s.SeedFromExport(nodes, edges)

	if added != 0 {
		t.Errorf("added = %d, want 0 (authoritative node must not be seeded)", added)
	}
	n := s.GetNode("mark://a:6309/a.md")
	if n.Title != "Local A" {
		t.Errorf("Title = %q, want %q", n.Title, "Local A")
	}
	if n.CrawledAt.IsZero() {
		t.Error("authoritative node lost its CrawledAt")
	}
	if bl := s.Backlinks("mark://a:6309/c.md"); len(bl) != 0 {
		t.Errorf("seed edges leaked past authoritative source: %v", bl)
	}
	if bl := s.Backlinks("mark://a:6309/b.md"); len(bl) != 1 {
		t.Errorf("local edge dropped by seed: %v", bl)
	}
}

func TestSeedFromExportOverwritesNonAuthoritative(t *testing.T) {
	s := New()

	// A locally crawled "error" node is not authoritative; seed may replace it.
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Status: "error"})
	s.Merge(g, nil)

	nodes := []StoredNode{{URL: "mark://a:6309/a.md", Title: "A", Status: "ok", LinkCount: 1}}
	added := s.SeedFromExport(nodes, nil)
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}
	n := s.GetNode("mark://a:6309/a.md")
	if n.Status != "ok" || n.Title != "A" {
		t.Errorf("node = %+v, want seeded ok/A", n)
	}
	if !n.CrawledAt.IsZero() {
		t.Error("seeded copy must carry zero CrawledAt")
	}
}

func TestSeedThenCrawlFlipsAuthoritative(t *testing.T) {
	s := New()

	nodes := []StoredNode{{URL: "mark://a:6309/a.md", Title: "A", Status: "ok", LinkCount: 1}}
	edges := []StoredEdge{{From: "mark://a:6309/a.md", To: "mark://a:6309/hub-only.md", Count: 1}}
	s.SeedFromExport(nodes, edges)

	// Local crawl of a.md observes a different outgoing set; Merge's refresh
	// logic must replace the seeded edges.
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://a:6309/a.md", Title: "A", Status: "ok", LinkCount: 1})
	g.AddEdgeInfo(graph.Edge{From: "mark://a:6309/a.md", To: "mark://a:6309/b.md"})
	s.Merge(g, nil)

	if bl := s.Backlinks("mark://a:6309/hub-only.md"); len(bl) != 0 {
		t.Errorf("stale seeded edge survived local crawl: %v", bl)
	}
	if bl := s.Backlinks("mark://a:6309/b.md"); len(bl) != 1 {
		t.Errorf("crawled edge missing: %v", bl)
	}

	// Now authoritative: a re-seed must not touch it.
	s.SeedFromExport(nodes, edges)
	if bl := s.Backlinks("mark://a:6309/hub-only.md"); len(bl) != 0 {
		t.Errorf("re-seed overwrote authoritative source: %v", bl)
	}
}

func TestSeedRefreshReplacesSeededEdges(t *testing.T) {
	s := New()

	first := []StoredEdge{{From: "mark://a:6309/a.md", To: "mark://a:6309/old.md", Count: 1}}
	s.SeedFromExport([]StoredNode{{URL: "mark://a:6309/a.md", Status: "ok"}}, first)

	// The hub aggregate no longer carries the old.md link.
	second := []StoredEdge{{From: "mark://a:6309/a.md", To: "mark://a:6309/new.md", Count: 1}}
	s.SeedFromExport([]StoredNode{{URL: "mark://a:6309/a.md", Status: "ok"}}, second)

	if bl := s.Backlinks("mark://a:6309/old.md"); len(bl) != 0 {
		t.Errorf("stale seeded backlink survived re-seed: %v", bl)
	}
	if bl := s.Backlinks("mark://a:6309/new.md"); len(bl) != 1 {
		t.Errorf("refreshed seed edge missing: %v", bl)
	}
}

func TestSeedEtagsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s.SetSeedEtag("a:6309", "etag-1")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if got := s2.SeedEtag("a:6309"); got != "etag-1" {
		t.Errorf("SeedEtag = %q, want %q", got, "etag-1")
	}
	if got := s2.SeedEtag("other:6309"); got != "" {
		t.Errorf("SeedEtag unknown host = %q, want empty", got)
	}
}

func TestLoadLegacyFileWithoutSeedEtags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	legacy := `{"version":1,"nodes":[],"edges":[]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if got := s.SeedEtag("a:6309"); got != "" {
		t.Errorf("SeedEtag = %q, want empty", got)
	}
	s.SetSeedEtag("a:6309", "etag-1")
	if err := s.Save(); err != nil {
		t.Fatalf("Save after legacy load: %v", err)
	}
}

func TestSeedFromLegacyExport(t *testing.T) {
	// A /graph.md still in the pre-enrichment two-column edge form must seed.
	body := `# Document Graph

> Exported: 2026-07-01T00:00:00Z
> Nodes: 2
> Edges: 1

## Nodes

| URL | Title | Status | Links |
|-----|-------|--------|-------|
| [mark://a:6309/a.md](mark://a:6309/a.md) | A | ok | 1 |
| [mark://a:6309/b.md](mark://a:6309/b.md) | B | ok | 0 |

## Edges

| From | To |
|------|----|
| mark://a:6309/a.md | mark://a:6309/b.md |
`
	nodes, edges := ParseExport(body)
	s := New()
	if added := s.SeedFromExport(nodes, edges); added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
	if bl := s.Backlinks("mark://a:6309/b.md"); len(bl) != 1 || bl[0] != "mark://a:6309/a.md" {
		t.Errorf("Backlinks = %v, want [mark://a:6309/a.md]", bl)
	}
}
