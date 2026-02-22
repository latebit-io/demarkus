package main

import (
	"strings"
	"testing"

	"github.com/latebit/demarkus/client/internal/graph"
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
