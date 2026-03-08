package graphstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/latebit/demarkus/client/internal/graph"
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

	fetchFunc := func(host, path string) (string, string, string, error) {
		key := host + path
		p, ok := pages[key]
		if !ok {
			return "not-found", "", "", nil
		}
		return "ok", p.body, p.etag, nil
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

	fetchFunc := func(_, _ string) (string, string, string, error) {
		return "ok", "# Doc\n", "etag-1", nil
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
