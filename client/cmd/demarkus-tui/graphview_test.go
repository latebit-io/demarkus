package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit/demarkus/client/internal/graph"
	"github.com/latebit/demarkus/client/internal/graphstore"
)

func TestFlattenGraphNilGraph(t *testing.T) {
	items := flattenGraph(nil, "mark://host/a.md")
	if items != nil {
		t.Errorf("expected nil, got %d items", len(items))
	}
}

func TestFlattenGraphMissingRoot(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/other.md", Title: "Other", Status: "ok"})
	items := flattenGraph(g, "mark://host/missing.md")
	if items != nil {
		t.Errorf("expected nil, got %d items", len(items))
	}
}

func TestFlattenGraphRootOnly(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/a.md", Title: "A", Status: "ok", Depth: 0})

	items := flattenGraph(g, "mark://host/a.md")
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].url != "mark://host/a.md" {
		t.Errorf("url = %q, want %q", items[0].url, "mark://host/a.md")
	}
	if items[0].title != "A" {
		t.Errorf("title = %q, want %q", items[0].title, "A")
	}
}

func TestFlattenGraphBFS(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/a.md", Title: "A", Status: "ok", Depth: 0})
	g.AddNode(&graph.Node{URL: "mark://host/b.md", Title: "B", Status: "ok", Depth: 1})
	g.AddNode(&graph.Node{URL: "mark://host/c.md", Title: "C", Status: "ok", Depth: 1})
	g.AddNode(&graph.Node{URL: "mark://host/d.md", Title: "D", Status: "ok", Depth: 2})
	g.AddEdge("mark://host/a.md", "mark://host/b.md")
	g.AddEdge("mark://host/a.md", "mark://host/c.md")
	g.AddEdge("mark://host/b.md", "mark://host/d.md")

	items := flattenGraph(g, "mark://host/a.md")
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	// Root first.
	if items[0].url != "mark://host/a.md" {
		t.Errorf("items[0].url = %q, want root", items[0].url)
	}
	// D should be last (depth 2).
	if items[3].url != "mark://host/d.md" {
		t.Errorf("items[3].url = %q, want d.md", items[3].url)
	}
}

func TestFlattenGraphNoCycles(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/a.md", Title: "A", Status: "ok", Depth: 0})
	g.AddNode(&graph.Node{URL: "mark://host/b.md", Title: "B", Status: "ok", Depth: 1})
	g.AddEdge("mark://host/a.md", "mark://host/b.md")
	g.AddEdge("mark://host/b.md", "mark://host/a.md") // cycle

	items := flattenGraph(g, "mark://host/a.md")
	if len(items) != 2 {
		t.Fatalf("expected 2 items (no duplicates from cycle), got %d", len(items))
	}
}

func TestRenderGraphViewEmpty(t *testing.T) {
	result := renderGraphView(nil, 0, 80)
	if !strings.Contains(result, "No nodes") {
		t.Errorf("expected empty message, got %q", result)
	}
}

func TestRenderGraphViewCursor(t *testing.T) {
	items := []graphListItem{
		{url: "mark://host/a.md", title: "A", status: "ok", depth: 0},
		{url: "mark://host/b.md", title: "B", status: "ok", depth: 1},
	}
	result := renderGraphView(items, 1, 80)
	lines := strings.Split(result, "\n")

	foundSelected := false
	for _, line := range lines {
		if strings.Contains(line, "B") && strings.HasPrefix(line, "> ") {
			foundSelected = true
		}
	}
	if !foundSelected {
		t.Errorf("expected selected cursor on item B, output:\n%s", result)
	}
}

func TestRenderGraphViewStatusIcons(t *testing.T) {
	items := []graphListItem{
		{url: "mark://host/a.md", title: "A", status: "ok", depth: 0},
		{url: "mark://host/b.md", title: "B", status: "not-found", depth: 1},
		{url: "https://ext.com", title: "", status: "external", depth: 1},
		{url: "mark://host/c.md", title: "C", status: "error", depth: 1},
	}
	result := renderGraphView(items, 0, 80)
	if !strings.Contains(result, "●") {
		t.Error("expected ● for ok status")
	}
	if !strings.Contains(result, "○") {
		t.Error("expected ○ for not-found status")
	}
	if !strings.Contains(result, "→") {
		t.Error("expected → for external status")
	}
	if !strings.Contains(result, "✗") {
		t.Error("expected ✗ for error status")
	}
}

func TestFlattenGraphBacklinkCounts(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://h/a.md", Title: "A", Depth: 0, Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://h/b.md", Title: "B", Depth: 1, Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://h/c.md", Title: "C", Depth: 1, Status: "ok"})
	g.AddEdge("mark://h/a.md", "mark://h/b.md")
	g.AddEdge("mark://h/a.md", "mark://h/c.md")
	g.AddEdge("mark://h/b.md", "mark://h/c.md")

	items := flattenGraph(g, "mark://h/a.md")
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}

	// a has 0 backlinks, b has 1, c has 2.
	counts := map[string]int{}
	for _, item := range items {
		counts[item.url] = item.backlinks
	}
	if counts["mark://h/a.md"] != 0 {
		t.Errorf("a.backlinks = %d, want 0", counts["mark://h/a.md"])
	}
	if counts["mark://h/b.md"] != 1 {
		t.Errorf("b.backlinks = %d, want 1", counts["mark://h/b.md"])
	}
	if counts["mark://h/c.md"] != 2 {
		t.Errorf("c.backlinks = %d, want 2", counts["mark://h/c.md"])
	}
}

func newTestStore(t *testing.T) *graphstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return gs
}

func TestBacklinksList(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		items := backlinksList(nil, "mark://h/a.md")
		if items != nil {
			t.Errorf("got %d items, want nil", len(items))
		}
	})

	t.Run("returns backlinks with titles", func(t *testing.T) {
		gs := newTestStore(t)
		g := graph.New()
		g.AddNode(&graph.Node{URL: "mark://h/a.md", Title: "A", Status: "ok"})
		g.AddNode(&graph.Node{URL: "mark://h/b.md", Title: "B", Status: "ok"})
		g.AddNode(&graph.Node{URL: "mark://h/c.md", Title: "C", Status: "ok"})
		g.AddEdge("mark://h/a.md", "mark://h/c.md")
		g.AddEdge("mark://h/b.md", "mark://h/c.md")
		gs.Merge(g, nil)

		items := backlinksList(gs, "mark://h/c.md")
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2", len(items))
		}
		urls := map[string]bool{}
		for _, item := range items {
			urls[item.url] = true
			if item.title == "" {
				t.Errorf("item %q has empty title", item.url)
			}
		}
		if !urls["mark://h/a.md"] || !urls["mark://h/b.md"] {
			t.Errorf("expected a.md and b.md, got %v", urls)
		}
	})

	t.Run("no backlinks", func(t *testing.T) {
		gs := newTestStore(t)
		g := graph.New()
		g.AddNode(&graph.Node{URL: "mark://h/a.md", Title: "A", Status: "ok"})
		gs.Merge(g, nil)

		items := backlinksList(gs, "mark://h/a.md")
		if len(items) != 0 {
			t.Errorf("got %d items, want 0", len(items))
		}
	})
}

func TestTopologyList(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		items := topologyList(nil)
		if items != nil {
			t.Errorf("got %d items, want nil", len(items))
		}
	})

	t.Run("sorted by backlink count descending", func(t *testing.T) {
		gs := newTestStore(t)
		g := graph.New()
		g.AddNode(&graph.Node{URL: "mark://h/a.md", Title: "A", Status: "ok"})
		g.AddNode(&graph.Node{URL: "mark://h/b.md", Title: "B", Status: "ok"})
		g.AddNode(&graph.Node{URL: "mark://h/c.md", Title: "C", Status: "ok"})
		g.AddEdge("mark://h/a.md", "mark://h/c.md")
		g.AddEdge("mark://h/b.md", "mark://h/c.md")
		g.AddEdge("mark://h/a.md", "mark://h/b.md")
		gs.Merge(g, nil)

		items := topologyList(gs)
		if len(items) != 3 {
			t.Fatalf("got %d items, want 3", len(items))
		}
		// c=2 backlinks, b=1, a=0
		if items[0].url != "mark://h/c.md" {
			t.Errorf("items[0] = %q, want c.md", items[0].url)
		}
		if items[0].backlinks != 2 {
			t.Errorf("items[0].backlinks = %d, want 2", items[0].backlinks)
		}
		if items[1].url != "mark://h/b.md" {
			t.Errorf("items[1] = %q, want b.md", items[1].url)
		}
		if items[2].url != "mark://h/a.md" {
			t.Errorf("items[2] = %q, want a.md", items[2].url)
		}
	})
}

func TestRenderDensityIndicator(t *testing.T) {
	items := []graphListItem{
		{url: "mark://h/a.md", title: "Root", status: "ok", depth: 0, backlinks: 5},
		{url: "mark://h/b.md", title: "Child", status: "ok", depth: 1, backlinks: 0},
	}
	out := renderGraphView(items, 0, 120)
	if !strings.Contains(out, "[5←]") {
		t.Error("missing density indicator [5←]")
	}
	if strings.Contains(out, "[0←]") {
		t.Error("should not show [0←] for zero backlinks")
	}
}

func TestRenderBacklinksView(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		out := renderBacklinksView(nil, 0, 80)
		if !strings.Contains(out, "No backlinks found") {
			t.Error("expected empty state message")
		}
	})

	t.Run("with items", func(t *testing.T) {
		items := []graphListItem{
			{url: "mark://h/a.md", title: "Page A", status: "ok"},
			{url: "mark://h/b.md", title: "Page B", status: "ok"},
		}
		out := renderBacklinksView(items, 1, 80)
		if !strings.Contains(out, "Backlinks") {
			t.Error("missing header")
		}
		if !strings.Contains(out, "> ") {
			t.Error("missing cursor on selected item")
		}
		if !strings.Contains(out, "Page A") || !strings.Contains(out, "Page B") {
			t.Error("missing item labels")
		}
	})
}

func TestRenderTopologyView(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		out := renderTopologyView(nil, 0, 80)
		if !strings.Contains(out, "No nodes in graph store") {
			t.Error("expected empty state message")
		}
	})

	t.Run("with items and density", func(t *testing.T) {
		items := []graphListItem{
			{url: "mark://h/a.md", title: "Popular", status: "ok", backlinks: 10},
			{url: "mark://h/b.md", title: "Lonely", status: "ok", backlinks: 0},
		}
		out := renderTopologyView(items, 0, 120)
		if !strings.Contains(out, "Topology") {
			t.Error("missing header")
		}
		if !strings.Contains(out, "[10←]") {
			t.Error("missing density indicator")
		}
	})
}

func TestStatusIcon(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"ok", "●"},
		{"not-found", "○"},
		{"error", "✗"},
		{"external", "→"},
		{"unknown", "○"},
		{"", "○"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := statusIcon(tt.status)
			if got != tt.want {
				t.Errorf("statusIcon(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestRenderGraphViewIndentation(t *testing.T) {
	items := []graphListItem{
		{url: "mark://host/a.md", title: "A", status: "ok", depth: 0},
		{url: "mark://host/b.md", title: "B", status: "ok", depth: 1},
		{url: "mark://host/c.md", title: "C", status: "ok", depth: 2},
	}
	result := renderGraphView(items, 0, 80)
	lines := strings.Split(result, "\n")

	// Find lines with B and C to check indentation increases.
	var bIndent, cIndent int
	for _, line := range lines {
		if strings.Contains(line, " B") {
			bIndent = len(line) - len(strings.TrimLeft(line, " >"))
		}
		if strings.Contains(line, " C") {
			cIndent = len(line) - len(strings.TrimLeft(line, " >"))
		}
	}
	if cIndent <= bIndent {
		t.Errorf("expected C to be more indented than B, got B=%d C=%d", bIndent, cIndent)
	}
}
