package main

import (
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
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
		if !strings.Contains(out, "No relations found") {
			t.Error("expected empty state message")
		}
	})

	t.Run("with items", func(t *testing.T) {
		items := []graphListItem{
			{url: "mark://h/a.md", title: "Page A", status: "ok"},
			{url: "mark://h/b.md", title: "Page B", status: "ok"},
		}
		out := renderBacklinksView(items, 1, 80)
		if !strings.Contains(out, "Relations") {
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

func TestGraphNeighborhoodListGroupsRelationsAndPaginates(t *testing.T) {
	store := newTestStore(t)
	edges := []graphstore.StoredEdge{
		{From: "mark://h/a.md", To: "mark://h/center.md", Rel: "depends-on", Count: 1},
		{From: "mark://h/center.md", To: "mark://h/a.md", Rel: "supersedes", Count: 1},
		{From: "mark://h/center.md", To: "mark://h/b.md", Count: 1},
	}
	store.ReplaceSeed("hub", nil, edges)
	first, err := graphNeighborhoodList(store, "mark://h/center.md", graphstore.NeighborhoodOptions{
		Direction: graphstore.NeighborhoodBoth, PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.items) != 1 || first.items[0].url != "mark://h/a.md" || len(first.items[0].edges) != 2 || first.nextCursor == "" || first.total != 2 {
		t.Fatalf("first page = %+v", first)
	}
	second, err := graphNeighborhoodList(store, "mark://h/center.md", graphstore.NeighborhoodOptions{
		Direction: graphstore.NeighborhoodBoth, PageSize: 1, Cursor: first.nextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.items) != 1 || second.items[0].url != "mark://h/b.md" || second.nextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	text := renderBacklinksView(first.items, 0, 120)
	for _, want := range []string{"incoming [depends-on] source mark://h/a.md", "outgoing [supersedes] source mark://h/center.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestHandleFetchResultCachesOnlyCurrentResponse(t *testing.T) {
	store := newTestStore(t)
	m := model{fetchSeq: 2, graphStore: store, addressBar: textinput.New(), histIdx: -1}
	response := fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK, Body: "# Source\n", Metadata: map[string]string{"version": "1", "rel-depends-on": "/target.md"},
	}}
	if _, _ = m.handleFetchResult(fetchResult{result: response, graphURL: "mark://h/source.md", url: "mark://h/source.md", seq: 1}); store.EdgeCount() != 0 {
		t.Fatal("stale fetch mutated graph cache")
	}
	_, save := m.handleFetchResult(fetchResult{result: response, graphURL: "mark://h/source.md", url: "mark://h/source.md", seq: 2})
	if store.EdgeCount() != 1 {
		t.Fatalf("current fetch edges = %d, want 1", store.EdgeCount())
	}
	if save == nil {
		t.Fatal("current fetch did not schedule graph save")
	}
	if result := save().(graphSaveResult); result.err != nil || result.generation != 1 {
		t.Fatalf("graph save: %v", result.err)
	}
}

func TestRelationRefreshWithNoBacklinksDoesNoGlobalWork(t *testing.T) {
	store := newTestStore(t)
	m := model{graphStore: store, crawlSeq: 4}
	msg := m.startRelationRefresh(t.Context(), "mark://h/target.md")().(relationRefreshResult)
	if msg.seq != 4 || msg.summary != "" || store.NodeCount() != 0 {
		t.Fatalf("empty refresh = %+v, nodes=%d", msg, store.NodeCount())
	}
}

func TestFlushGraphStorePersistsPendingGeneration(t *testing.T) {
	store := newTestStore(t)
	store.ObserveDocument("mark://h/source.md", graph.FetchResult{
		Status: "ok", Metadata: map[string]string{"version": "1", "rel-depends-on": "/target.md"},
	})
	m := model{graphStore: store, graphGeneration: 1}
	m.flushGraphStore()
	if m.graphSavedGeneration != 1 {
		t.Fatalf("saved generation = %d, want 1", m.graphSavedGeneration)
	}
}

func TestRelationsToLinksStartsFreshCrawl(t *testing.T) {
	address := textinput.New()
	address.SetValue("mark://h/target.md")
	m := model{graphStore: newTestStore(t), addressBar: address, viewMode: viewGraph, graphSubView: subViewBacklinks}
	updated, cmd := m.handleGraphKey(tea.KeyPressMsg{Code: 'd'})
	got := updated.(model)
	if cmd == nil || !got.crawling || got.graphSubView != subViewLinks {
		t.Fatalf("links transition: cmd=%v crawling=%t subview=%v", cmd != nil, got.crawling, got.graphSubView)
	}
	got.cancelCrawl()
}

func TestRelationsToggleUsesCacheWithoutCrawl(t *testing.T) {
	store := newTestStore(t)
	store.ReplaceSeed("hub", nil, []graphstore.StoredEdge{{From: "mark://h/source.md", To: "mark://h/target.md", Rel: "depends-on", Count: 1}})
	address := textinput.New()
	address.SetValue("mark://h/target.md")
	m := model{graphStore: store, addressBar: address}
	updated, cmd := m.handleRelationsToggle()
	got := updated.(model)
	if cmd != nil || got.crawling || got.viewMode != viewGraph || got.graphSubView != subViewBacklinks {
		t.Fatalf("relations toggle started work: cmd=%v crawling=%t mode=%v subview=%v", cmd != nil, got.crawling, got.viewMode, got.graphSubView)
	}
	if len(got.graphNodes) != 1 || got.graphNodes[0].url != "mark://h/source.md" {
		t.Fatalf("cached relation rows = %+v", got.graphNodes)
	}
}

func TestRelationsToggleClearsPreviousPageState(t *testing.T) {
	address := textinput.New()
	address.SetValue("mark://h/target.md")
	m := model{
		addressBar:      address,
		graphNodes:      []graphListItem{{url: "mark://h/stale.md"}},
		graphPageCursor: "stale-cursor",
		graphPageNext:   "stale-next",
		graphPageTotal:  1,
		graphIdx:        1,
	}
	updated, _ := m.handleRelationsToggle()
	got := updated.(model)
	if len(got.graphNodes) != 0 || got.graphPageCursor != "" || got.graphPageNext != "" || got.graphPageTotal != 0 || got.graphIdx != 0 {
		t.Fatalf("stale relation page retained: nodes=%+v cursor=%q next=%q total=%d idx=%d",
			got.graphNodes, got.graphPageCursor, got.graphPageNext, got.graphPageTotal, got.graphIdx)
	}
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

func TestTruncateCells(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{"no truncation", "hello", 10, "hello"},
		{"exact fit", "hello", 5, "hello"},
		{"truncate ASCII", "hello world", 8, "hello..."},
		// CJK is unambiguously double-width; results are stable regardless
		// of go-runewidth's East Asian mode (● and ← are ambiguous-width,
		// so they stay out of assertions).
		{"truncate CJK", "你好世界超宽", 9, "你好世..."},
		{"CJK exact fit", "你好", 4, "你好"},
		{"very short max", "hello", 2, "he"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateCells(tt.input, tt.max)
			if got != tt.expected {
				t.Errorf("truncateCells(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
			}
		})
	}
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
