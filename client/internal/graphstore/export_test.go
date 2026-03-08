package graphstore

import (
	"strings"
	"testing"
	"time"
)

func TestExport(t *testing.T) {
	s := &Store{
		nodes: map[string]*StoredNode{
			"mark://a:6309/x.md": {
				URL: "mark://a:6309/x.md", Title: "Page X",
				Status: "ok", LinkCount: 2, CrawledAt: time.Now(),
			},
			"mark://a:6309/y.md": {
				URL: "mark://a:6309/y.md", Title: "Page Y",
				Status: "ok", LinkCount: 1, CrawledAt: time.Now(),
			},
		},
		edges: []StoredEdge{
			{From: "mark://a:6309/x.md", To: "mark://a:6309/y.md"},
		},
		edgeSet: map[StoredEdge]struct{}{
			{From: "mark://a:6309/x.md", To: "mark://a:6309/y.md"}: {},
		},
	}

	md := s.Export()

	// Check structure.
	if !strings.Contains(md, "# Document Graph") {
		t.Error("missing title")
	}
	if !strings.Contains(md, "> Nodes: 2") {
		t.Error("missing node count")
	}
	if !strings.Contains(md, "> Edges: 1") {
		t.Error("missing edge count")
	}
	if !strings.Contains(md, "[mark://a:6309/x.md](mark://a:6309/x.md)") {
		t.Error("missing node link for x.md")
	}
	if !strings.Contains(md, "## Edges") {
		t.Error("missing edges section")
	}

	// Nodes should be sorted by URL.
	xPos := strings.Index(md, "mark://a:6309/x.md")
	yPos := strings.Index(md, "mark://a:6309/y.md")
	if xPos > yPos {
		t.Error("nodes not sorted by URL")
	}
}

func TestExportEmpty(t *testing.T) {
	s := &Store{
		nodes:   make(map[string]*StoredNode),
		edgeSet: make(map[StoredEdge]struct{}),
	}

	md := s.Export()

	if !strings.Contains(md, "> Nodes: 0") {
		t.Error("expected zero nodes")
	}
	if !strings.Contains(md, "> Edges: 0") {
		t.Error("expected zero edges")
	}
	if strings.Contains(md, "## Edges") {
		t.Error("edges section should be omitted when empty")
	}
}

func TestParseExport(t *testing.T) {
	md := `# Document Graph

> Exported: 2026-03-08T12:00:00Z
> Nodes: 2
> Edges: 1

## Nodes

| URL | Title | Status | Links |
|-----|-------|--------|-------|
| [mark://a:6309/x.md](mark://a:6309/x.md) | Page X | ok | 2 |
| [mark://a:6309/y.md](mark://a:6309/y.md) | Page Y | ok | 1 |

## Edges

| From | To |
|------|----|
| mark://a:6309/x.md | mark://a:6309/y.md |
`

	nodes, edges := ParseExport(md)

	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
	if nodes[0].URL != "mark://a:6309/x.md" {
		t.Errorf("nodes[0].URL = %q, want mark://a:6309/x.md", nodes[0].URL)
	}
	if nodes[0].Title != "Page X" {
		t.Errorf("nodes[0].Title = %q, want Page X", nodes[0].Title)
	}
	if nodes[0].Status != "ok" {
		t.Errorf("nodes[0].Status = %q, want ok", nodes[0].Status)
	}
	if nodes[0].LinkCount != 2 {
		t.Errorf("nodes[0].LinkCount = %d, want 2", nodes[0].LinkCount)
	}

	if len(edges) != 1 {
		t.Fatalf("len(edges) = %d, want 1", len(edges))
	}
	if edges[0].From != "mark://a:6309/x.md" || edges[0].To != "mark://a:6309/y.md" {
		t.Errorf("edge = %+v, want x.md -> y.md", edges[0])
	}
}

func TestExportParseRoundTrip(t *testing.T) {
	s := &Store{
		nodes: map[string]*StoredNode{
			"mark://a:6309/a.md": {
				URL: "mark://a:6309/a.md", Title: "Doc A | Appendix",
				Status: "ok", LinkCount: 3, CrawledAt: time.Now(),
			},
			"mark://b:6309/b.md": {
				URL: "mark://b:6309/b.md", Title: "Doc B",
				Status: "ok", LinkCount: 1, CrawledAt: time.Now(),
			},
			"https://example.com": {
				URL: "https://example.com", Title: "",
				Status: "external", LinkCount: 0, CrawledAt: time.Now(),
			},
		},
		edges: []StoredEdge{
			{From: "mark://a:6309/a.md", To: "mark://b:6309/b.md"},
			{From: "mark://a:6309/a.md", To: "https://example.com"},
		},
		edgeSet: map[StoredEdge]struct{}{
			{From: "mark://a:6309/a.md", To: "mark://b:6309/b.md"}:  {},
			{From: "mark://a:6309/a.md", To: "https://example.com"}: {},
		},
	}

	md := s.Export()
	nodes, edges := ParseExport(md)

	if len(nodes) != 3 {
		t.Fatalf("round-trip nodes: got %d, want 3", len(nodes))
	}
	if len(edges) != 2 {
		t.Fatalf("round-trip edges: got %d, want 2", len(edges))
	}

	// Verify all original nodes are recovered.
	nodeMap := make(map[string]StoredNode)
	for _, n := range nodes {
		nodeMap[n.URL] = n
	}
	for url, orig := range s.nodes {
		got, ok := nodeMap[url]
		if !ok {
			t.Errorf("missing node %q after round-trip", url)
			continue
		}
		if got.Title != orig.Title {
			t.Errorf("node %q title: got %q, want %q", url, got.Title, orig.Title)
		}
		if got.Status != orig.Status {
			t.Errorf("node %q status: got %q, want %q", url, got.Status, orig.Status)
		}
		if got.LinkCount != orig.LinkCount {
			t.Errorf("node %q link_count: got %d, want %d", url, got.LinkCount, orig.LinkCount)
		}
	}
}
