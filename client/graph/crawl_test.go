package graph

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/links"
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

func (m *mockFetcher) addWithMeta(host, path, body string, meta map[string]string) {
	m.pages[host+path] = FetchResult{Status: "ok", Body: body, Metadata: meta}
}

func (m *mockFetcher) Fetch(_ context.Context, target links.Target) (FetchResult, error) {
	host, path := target.DialHost(), target.Path
	m.mu.Lock()
	m.calls = append(m.calls, host+path)
	m.mu.Unlock()

	r, ok := m.pages[host+path]
	if !ok {
		return FetchResult{Status: "not-found"}, nil
	}
	return r, nil
}

func TestCrawlSinglePage(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\nNo links here.")

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if g.NodeCount() != 1 {
		t.Fatalf("NodeCount() = %d, want 1", g.NodeCount())
	}
	n := g.GetNode("mark://host/index.md")
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

// A slow sibling must not let a longer path claim the depth-boundary node.
func TestCrawlCompetingDepths(t *testing.T) {
	f := &competingFetcher{mockFetcher: newMockFetcher()}
	f.add("host:6309", "/root.md", "[long](/long.md) [short](/short.md)")
	f.add("host:6309", "/long.md", "[middle](/middle.md)")
	f.add("host:6309", "/middle.md", "[join](/join.md)")
	f.add("host:6309", "/short.md", "[join](/join.md)")
	f.add("host:6309", "/join.md", "[leaf](/leaf.md)")
	f.add("host:6309", "/leaf.md", "# Leaf")
	g, err := Crawl(t.Context(), "mark://host/root.md", f, CrawlOptions{MaxDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if n := g.GetNode("mark://host/join.md"); n == nil || n.Depth != 2 {
		t.Errorf("join = %+v, want depth 2", n)
	}
	if n := g.GetNode("mark://host/leaf.md"); n == nil || n.Depth != 3 {
		t.Errorf("leaf = %+v, want depth 3", n)
	}
}

type competingFetcher struct{ *mockFetcher }

func (f *competingFetcher) Fetch(ctx context.Context, target links.Target) (FetchResult, error) {
	if target.Path == "/short.md" {
		time.Sleep(50 * time.Millisecond)
	}
	return f.mockFetcher.Fetch(ctx, target)
}

func TestExtractDocumentEdges(t *testing.T) {
	extracted := ExtractDocumentEdges(
		"mark://host:6309/docs/deep/source.md",
		"# Heading Title\n\n## References\n\n[first](sibling.md) [second](sibling.md) [parent](../peer.md) [root](/root.md) [web](https://example.com/doc)\n",
		map[string]string{
			"title":          "Declared Title",
			"rel-related":    "../old.md, mark://foreign:6309/typed.md",
			"rel-depends-on": "/root.md",
		},
	)

	if extracted.BodyLinkCount != 5 {
		t.Errorf("BodyLinkCount = %d, want 5", extracted.BodyLinkCount)
	}
	want := []Edge{
		{From: "mark://host/docs/deep/source.md", To: "mark://host/docs/deep/sibling.md", Label: "first", Anchor: "references", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://host/docs/deep/sibling.md", Label: "second", Anchor: "references", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://host/docs/peer.md", Label: "parent", Anchor: "references", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://host/root.md", Label: "root", Anchor: "references", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "https://example.com/doc", Label: "web", Anchor: "references", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://host/root.md", Rel: "depends-on", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://host/docs/old.md", Rel: "related", Count: 1},
		{From: "mark://host/docs/deep/source.md", To: "mark://foreign/typed.md", Rel: "related", Count: 1},
	}
	if !slices.Equal(extracted.Edges, want) {
		t.Errorf("Edges = %+v, want %+v", extracted.Edges, want)
	}

}

func TestCrawlFollowsLinks(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\nGo to [about](about.md).")
	f.add("host:6309", "/about.md", "# About\n\nBack to [home](index.md).")

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	if g.NodeCount() != 2 {
		t.Fatalf("NodeCount() = %d, want 2", g.NodeCount())
	}
	if g.EdgeCount() != 2 {
		t.Fatalf("EdgeCount() = %d, want 2", g.EdgeCount())
	}

	about := g.GetNode("mark://host/about.md")
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

	g, err := Crawl(context.Background(), "mark://host/a.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	// a(0) -> b(1) -> c(2) -> d would be depth 3, should not be crawled
	if g.GetNode("mark://host/a.md") == nil {
		t.Error("a not found")
	}
	if g.GetNode("mark://host/b.md") == nil {
		t.Error("b not found")
	}
	if g.GetNode("mark://host/c.md") == nil {
		t.Error("c not found")
	}
	// d should not be fetched (depth 3), but it may exist as an unfetched node
	// since c links to it — however c is at depth 2 so its links are NOT followed
	if n := g.GetNode("mark://host/d.md"); n != nil {
		t.Errorf("d should not be discovered, but found with status %q", n.Status)
	}
}

func TestCrawlHandlesNotFound(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\n[missing](missing.md)")
	// missing.md is not added, so it returns not-found

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	missing := g.GetNode("mark://host/missing.md")
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

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
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

	g, err := Crawl(ctx, "mark://host/a.md", f, CrawlOptions{MaxDepth: 2})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Crawl() error = %v, want cancelled incomplete outcome", err)
	}

	if g.NodeCount() != 0 || len(f.calls) != 0 || g.Outcome.Complete {
		t.Fatalf("pre-cancelled crawl did work: %+v", g.Outcome)
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

	g, err := Crawl(context.Background(), "mark://host/a.md", f, CrawlOptions{MaxDepth: 2, OnNode: onNode})
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

	g, err := Crawl(context.Background(), "mark://host/a.md", f, CrawlOptions{MaxDepth: 10})
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

func findEdge(t *testing.T, g *Graph, from, to, rel string) Edge {
	t.Helper()
	for _, e := range g.GetEdges() {
		if e.From == from && e.To == to && e.Rel == rel {
			return e
		}
	}
	t.Fatalf("edge %s -> %s [%s] not found in %+v", from, to, rel, g.GetEdges())
	return Edge{}
}

func TestCrawlRecordsLabelAndAnchor(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\n## Section\n\nSee [About page](about.md).")
	f.add("host:6309", "/about.md", "# About\n")

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	e := findEdge(t, g, "mark://host/index.md", "mark://host/about.md", "")
	if e.Label != "About page" || e.Anchor != "section" || e.Count != 1 {
		t.Errorf("edge = %+v, want Label=About page Anchor=section Count=1", e)
	}
}

func TestCrawlRecordsLinkInsideInlineFormatting(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/index.md", "# Home\n\n## Section\n\n**[Bold link](about.md)**")
	f.add("host:6309", "/about.md", "# About\n")

	g, err := Crawl(context.Background(), "mark://host/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	e := findEdge(t, g, "mark://host/index.md", "mark://host/about.md", "")
	if e.Label != "Bold link" || e.Anchor != "section" {
		t.Errorf("edge = %+v, want Label=Bold link Anchor=section", e)
	}
}

func TestCrawlAggregatesRepeatedLinks(t *testing.T) {
	f := newMockFetcher()
	f.addWithMeta("host:6309", "/docs/index.md", "# Heading Home\n\n[first](about.md) then [second](about.md)", map[string]string{"title": "Declared Home"})
	f.add("host:6309", "/docs/about.md", "# About\n")

	g, err := Crawl(context.Background(), "mark://host:6309/docs/index.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	e := findEdge(t, g, "mark://host/docs/index.md", "mark://host/docs/about.md", "")
	if e.Count != 2 {
		t.Errorf("Count = %d, want 2", e.Count)
	}
	if e.Label != "first" {
		t.Errorf("Label = %q, want first (first label wins)", e.Label)
	}
	if n := g.GetNode("mark://host/docs/index.md"); n.LinkCount != 2 || n.Title != "Heading Home" {
		t.Errorf("node = %+v, want LinkCount 2 and heading title", n)
	}
}

func TestCrawlIngestsRelMetadata(t *testing.T) {
	f := newMockFetcher()
	f.addWithMeta("host:6309", "/adr/0004.md", "# ADR 0004\n", map[string]string{
		"rel-supersedes": "/adr/0002.md, /adr/0003.md",
		"title":          "ADR 0004",
	})
	f.add("host:6309", "/adr/0002.md", "# ADR 0002\n")
	f.add("host:6309", "/adr/0003.md", "# ADR 0003\n")

	g, err := Crawl(context.Background(), "mark://host/adr/0004.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}

	for _, target := range []string{"mark://host/adr/0002.md", "mark://host/adr/0003.md"} {
		e := findEdge(t, g, "mark://host/adr/0004.md", target, "supersedes")
		if e.Count != 1 || e.Label != "" {
			t.Errorf("edge = %+v, want Count=1 empty Label", e)
		}
		n := g.GetNode(target)
		if n == nil || n.Status != "ok" {
			t.Errorf("rel target %s not crawled: %+v", target, n)
		}
	}

	// rel- edges do not count as body links.
	if n := g.GetNode("mark://host/adr/0004.md"); n.LinkCount != 0 {
		t.Errorf("LinkCount = %d, want 0", n.LinkCount)
	}
}

func TestCrawlSkipsMalformedRelRefs(t *testing.T) {
	f := newMockFetcher()
	f.addWithMeta("host:6309", "/doc.md", "# Doc\n", map[string]string{
		"rel-supersedes": " , bad ref with spaces, bad\nnewline",
		"rel-":           "/other.md",
		"rel-self":       "/doc.md",
	})

	g, err := Crawl(context.Background(), "mark://host/doc.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}
	if g.EdgeCount() != 0 {
		t.Errorf("EdgeCount = %d, want 0: %+v", g.EdgeCount(), g.GetEdges())
	}
}

// A rel- target that points back at the document is a self-reference even when
// the caller addressed the document by its dial address. fedcrawl does exactly
// that, so comparing raw strings would emit a bogus self-edge.
func TestRelEdgesSkipsSelfReferenceAcrossURLForms(t *testing.T) {
	for _, docURL := range []string{"mark://host:6309/doc.md", "mark://host/doc.md"} {
		refs := RelEdges(docURL, map[string]string{"rel-supersedes": "/doc.md, /other.md"}).Refs
		if len(refs) != 1 {
			t.Fatalf("RelEdges(%q) = %+v, want only the non-self target", docURL, refs)
		}
		if refs[0].Target != "mark://host/other.md" {
			t.Errorf("target = %q, want mark://host/other.md", refs[0].Target)
		}
	}
}

// A bracketed IPv6 authority carries the default port for dialing and drops it
// for identity, the same as any other host.
func TestCrawlIPv6Identity(t *testing.T) {
	f := newMockFetcher()
	f.add("[::1]:6309", "/doc.md", "# Doc\n\n[Other](/other.md)\n")
	f.add("[::1]:6309", "/other.md", "# Other\n")

	g, err := Crawl(context.Background(), "mark://[::1]/doc.md", f, CrawlOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Crawl() error: %v", err)
	}
	n := g.GetNode("mark://[::1]/doc.md")
	if n == nil || n.Status != "ok" {
		t.Fatalf("node = %+v, want an ok node under the identity key", n)
	}
	if g.GetNode("mark://[::1]/other.md") == nil {
		t.Error("linked IPv6 node missing under its identity key")
	}
	for _, e := range g.GetEdges() {
		if strings.Contains(e.From, ":6309") || strings.Contains(e.To, ":6309") {
			t.Errorf("edge %s -> %s kept the dial port", e.From, e.To)
		}
	}
}
