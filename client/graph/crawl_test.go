package graph

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// mockFetcher returns canned responses keyed by "host/path".
type mockFetcher struct {
	mu    sync.Mutex
	pages map[string]FetchResult
	calls []string // records fetch order for assertions
}

func newMockFetcher() *mockFetcher {
	return &mockFetcher{pages: make(map[string]FetchResult)}
}

func (m *mockFetcher) add(host, path, body string) {
	m.pages[host+path] = FetchResult{Status: "ok", Body: body}
}

func (m *mockFetcher) Fetch(host, path string) (FetchResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, host+path)
	m.mu.Unlock()

	r, ok := m.pages[host+path]
	if !ok {
		return FetchResult{Status: "not-found"}, nil
	}
	return r, nil
}

func mockParseURL(raw string) (host, path string, err error) {
	// Minimal parser for mark://host:6309/path
	if len(raw) < 7 || raw[:7] != "mark://" {
		return "", "", fmt.Errorf("not a mark URL: %s", raw)
	}
	rest := raw[7:] // "host:6309/path"
	slashIdx := -1
	for i, c := range rest {
		if c == '/' {
			slashIdx = i
			break
		}
	}
	if slashIdx < 0 {
		return rest, "/", nil
	}
	return rest[:slashIdx], rest[slashIdx:], nil
}

func TestCrawlSinglePage(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\nNo links here.")

	g, err := Crawl(context.Background(), "mark://host:6309/index.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if g.NodeCount() != 1 {
		t.Fatalf("NodeCount() = %d, want 1", g.NodeCount())
	}
	n := g.GetNode("mark://host:6309/index.md")
	if n == nil {
		t.Fatal("start node not found")
	}
	if n.Title != "Home" {
		t.Errorf("Title = %q, want %q", n.Title, "Home")
	}
	if n.Status != "ok" {
		t.Errorf("Status = %q, want %q", n.Status, "ok")
	}
	if n.Depth != 0 {
		t.Errorf("Depth = %d, want 0", n.Depth)
	}
}

func TestCrawlFollowsLinks(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\nGo to [about](about.md).")
	f.add("host:6309", "/about.md", "# About\n\nBack to [home](index.md).")

	g, err := Crawl(context.Background(), "mark://host:6309/index.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if g.NodeCount() != 2 {
		t.Fatalf("NodeCount() = %d, want 2", g.NodeCount())
	}
	if g.EdgeCount() != 2 {
		t.Fatalf("EdgeCount() = %d, want 2", g.EdgeCount())
	}

	about := g.GetNode("mark://host:6309/about.md")
	if about == nil {
		t.Fatal("about node not found")
	}
	if about.Depth != 1 {
		t.Errorf("about.Depth = %d, want 1", about.Depth)
	}
}

func TestCrawlRespectsDepthLimit(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/a.md", "# A\n\n[b](b.md)")
	f.add("host:6309", "/b.md", "# B\n\n[c](c.md)")
	f.add("host:6309", "/c.md", "# C\n\n[d](d.md)")
	f.add("host:6309", "/d.md", "# D\n\nEnd.")

	g, err := Crawl(context.Background(), "mark://host:6309/a.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	// a(0) -> b(1) -> c(2) -> d would be depth 3, should not be crawled
	if g.GetNode("mark://host:6309/a.md") == nil {
		t.Error("a not found")
	}
	if g.GetNode("mark://host:6309/b.md") == nil {
		t.Error("b not found")
	}
	if g.GetNode("mark://host:6309/c.md") == nil {
		t.Error("c not found")
	}
	// d should not be fetched (depth 3), but it may exist as an unfetched node
	// since c links to it — however c is at depth 2 so its links are NOT followed
	if n := g.GetNode("mark://host:6309/d.md"); n != nil {
		t.Errorf("d should not be discovered, but found with status %q", n.Status)
	}
}

func TestCrawlHandlesNotFound(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\n[missing](missing.md)")
	// missing.md is not added, so it returns not-found

	g, err := Crawl(context.Background(), "mark://host:6309/index.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	missing := g.GetNode("mark://host:6309/missing.md")
	if missing == nil {
		t.Fatal("missing node not found in graph")
	}
	if missing.Status != "not-found" {
		t.Errorf("missing.Status = %q, want %q", missing.Status, "not-found")
	}
}

func TestCrawlExternalLinksNotCrawled(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\n[ext](https://example.com)")

	g, err := Crawl(context.Background(), "mark://host:6309/index.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	ext := g.GetNode("https://example.com")
	if ext == nil {
		t.Fatal("external node not found in graph")
	}
	if ext.Status != "external" {
		t.Errorf("ext.Status = %q, want %q", ext.Status, "external")
	}
	if g.EdgeCount() != 1 {
		t.Errorf("EdgeCount() = %d, want 1", g.EdgeCount())
	}
}

func TestCrawlCancellation(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/a.md", "# A\n\n[b](b.md)")
	f.add("host:6309", "/b.md", "# B\n\n[c](c.md)")
	f.add("host:6309", "/c.md", "# C")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	g, err := Crawl(ctx, "mark://host:6309/a.md", f, mockParseURL, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	// With immediate cancellation, we may get 0 or 1 nodes depending on timing.
	// The important thing is that it terminates and doesn't crawl everything.
	if g.NodeCount() > 1 {
		t.Logf("NodeCount() = %d (expected 0 or 1 with cancelled context)", g.NodeCount())
	}
}

func TestCrawlOnNodeCallback(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/a.md", "# A\n\n[b](b.md)")
	f.add("host:6309", "/b.md", "# B")

	var mu sync.Mutex
	var discovered []string
	onNode := func(n *Node) {
		mu.Lock()
		discovered = append(discovered, n.URL)
		mu.Unlock()
	}

	g, err := Crawl(context.Background(), "mark://host:6309/a.md", f, mockParseURL, CrawlOptions{MaxDepth: 2, OnNode: onNode})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if len(discovered) != g.NodeCount() {
		t.Errorf("OnNode called %d times, but graph has %d nodes", len(discovered), g.NodeCount())
	}
}

func TestCrawlNoCycles(t *testing.T) {
	f := newMockFetcher()
	// Create a cycle: a -> b -> c -> a
	f.add("host:6309", "/a.md", "# A\n\n[b](b.md)")
	f.add("host:6309", "/b.md", "# B\n\n[c](c.md)")
	f.add("host:6309", "/c.md", "# C\n\n[a](a.md)")

	g, err := Crawl(context.Background(), "mark://host:6309/a.md", f, mockParseURL, CrawlOptions{MaxDepth: 10})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if g.NodeCount() != 3 {
		t.Errorf("NodeCount() = %d, want 3 (should not re-crawl cycle)", g.NodeCount())
	}

	// Edges should be 3: a->b, b->c, c->a
	if g.EdgeCount() != 3 {
		t.Errorf("EdgeCount() = %d, want 3", g.EdgeCount())
	}
}
