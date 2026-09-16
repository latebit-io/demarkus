package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

const exploreDoc = `# Hub

The hub links everything together.

## Sections

- [Alpha](/alpha.md)
- [Beta](/docs/beta.md)

## More

See [Alpha](/alpha.md) again (deduped).
`

const exploreListing = `- [alpha.md](alpha.md)
- [docs/](docs/)
- [hub.md](hub.md)
- [notes.md](notes.md)
`

func exploreStub() *stubClient {
	return &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "2", "modified": "2026-07-04T00:00:00Z", "etag": "xyz"},
				Body:     exploreDoc,
			}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   exploreListing,
			}}, nil
		},
	}
}

func exploreText(t *testing.T, h *handler, url string) string {
	t.Helper()
	result, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{"url": url}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	return result.Content[0].(mcp.TextContent).Text
}

func TestHandlerMarkExplore_Card(t *testing.T) {
	h := &handler{client: exploreStub()}
	text := exploreText(t, h, "mark://host:6309/hub.md")

	wants := []string{
		"status: ok",
		"version: 2",
		"size: ",
		"## Outline",
		"- Hub (#hub,",
		"  - Sections (#sections,",
		"## Opening",
		"The hub links everything together.",
		"## Outbound links (2)",
		"- [Alpha](/alpha.md)",
		"- [Beta](/docs/beta.md)",
		"## Backlinks",
		"(graph store unavailable)",
		"## Siblings in / (3)",
		"- alpha.md",
		"- notes.md",
		"- docs/",
		"fetch mark://host:6309/hub.md#<anchor> for a section",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("card missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "- hub.md") {
		t.Error("siblings must exclude the document itself")
	}
	// /alpha.md is linked twice in the doc; outbound dedup lists it once.
	if strings.Count(text, "(/alpha.md)") != 1 {
		t.Errorf("outbound links should be deduplicated:\n%s", text)
	}
}

func TestHandlerMarkExplore_BacklinksFromStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := graph.New()
	observation := graph.Observe("mark://host/a.md", map[string]string{"version": "2"})
	observation.Complete = true
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok", Observation: observation})
	g.AddNode(&graph.Node{URL: "mark://host:6309/hub.md", Title: "Hub", Status: "ok"})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/hub.md")
	gs.Merge(g, nil)

	h := &handler{client: exploreStub(), graphStore: gs}
	text := exploreText(t, h, "mark://host:6309/hub.md")

	// The card shows node identity, which omits the default port (ADR 0005),
	// even though the caller addressed the document by its dial address.
	for _, want := range []string{"## Backlinks (1)", "[Page A](mark://host/a.md)", "freshness: fresh"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected backlink %q in card:\n%s", want, text)
		}
	}
	if strings.Contains(text, "## Relations") || strings.Contains(text, "[source](") {
		t.Fatalf("default card expanded the relation neighborhood:\n%s", text)
	}
}

func TestHandlerMarkExplore_OrdinaryReadCachesTypedRelations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	gs, err := graphstore.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := exploreStub()
	sc.fetchFn = func(_, _, _ string) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"version": "3", "etag": "v3", "rel-supersedes": "/old.md", "rel-depends-on": "/base.md",
			},
			Body: "# Current\n\n[Body](/body.md)\n",
		}}, nil
	}
	h := &handler{client: sc, graphStore: gs}
	result, callErr := h.markExplore(context.Background(), newCallToolRequest(map[string]any{
		"url": "mark://host:6309/current.md", "direction": "outgoing", "relations": []string{"supersedes"},
	}))
	if callErr != nil || result.IsError {
		t.Fatalf("markExplore: err=%v result=%+v", callErr, result)
	}
	text := result.Content[0].(mcp.TextContent).Text
	for _, want := range []string{"## Relations (1 documents)", "mark://host/old.md", "outgoing [supersedes]", "[source](mark://host/current.md)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "mark://host/base.md") || strings.Contains(text, "mark://host/body.md") {
		t.Errorf("relation filter leaked unmatched rows:\n%s", text)
	}

	reloaded, err := graphstore.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reloaded.Neighborhood("mark://host/current.md", graphstore.NeighborhoodOptions{Direction: graphstore.NeighborhoodOutgoing})
	if err != nil || page.TotalRows != 3 {
		t.Fatalf("cold persisted neighborhood = %+v, %v", page, err)
	}
}

func TestHandlerMarkExplore_RelationPagination(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Current\n\n")
	for i := range 5 {
		fmt.Fprintf(&body, "[%02d](/%02d.md)\n", i, i)
	}
	sc := exploreStub()
	sc.fetchFn = func(_, _, _ string) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}, Body: body.String()}}, nil
	}
	h := &handler{client: sc, graphStore: graphstore.New()}
	first, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{
		"url": "mark://host/current.md", "direction": "outgoing", "page_size": 2,
	}))
	if err != nil || first.IsError {
		t.Fatalf("first page: err=%v result=%+v", err, first)
	}
	firstText := first.Content[0].(mcp.TextContent).Text
	cursor := lineValue(firstText, "next-cursor: ")
	if cursor == "" || !strings.Contains(firstText, "mark://host/00.md") || strings.Contains(firstText, "mark://host/02.md") {
		t.Fatalf("first page not bounded:\n%s", firstText)
	}
	second, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{
		"url": "mark://host/current.md", "direction": "outgoing", "page_size": 2, "cursor": cursor,
	}))
	if err != nil || second.IsError {
		t.Fatalf("second page: err=%v result=%+v", err, second)
	}
	secondText := second.Content[0].(mcp.TextContent).Text
	if !strings.Contains(secondText, "mark://host/02.md") || strings.Contains(secondText, "mark://host/00.md") {
		t.Fatalf("second page did not continue:\n%s", secondText)
	}
}

func lineValue(text, prefix string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return value
		}
	}
	return ""
}

func TestHandlerMarkExplore_SectionCaps(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Big hub\n\n")
	for i := range 15 {
		fmt.Fprintf(&body, "## Section %d\n\n", i)
		fmt.Fprintf(&body, "- [Doc %d](/doc-%d.md)\n\n", i, i)
	}
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1"},
				Body:     body.String(),
			}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: ""}}, nil
		},
	}
	h := &handler{client: sc}
	text := exploreText(t, h, "mark://host:6309/hub.md")

	if !strings.Contains(text, "+6 more headings") {
		t.Errorf("expected heading overflow marker (16 headings, cap 10):\n%s", text)
	}
	if !strings.Contains(text, "+5 more links") {
		t.Errorf("expected link overflow marker (15 links, cap 10):\n%s", text)
	}
	if strings.Contains(text, "Section 12") && strings.Contains(text, "#section-12") {
		t.Error("headings past the cap should not be listed")
	}
}

func TestHandlerMarkExplore_ListFailureDegrades(t *testing.T) {
	sc := exploreStub()
	sc.listFn = func(_, _, _ string) (fetch.Result, error) {
		return fetch.Result{}, fmt.Errorf("boom")
	}
	h := &handler{client: sc}
	text := exploreText(t, h, "mark://host:6309/hub.md")
	if !strings.Contains(text, "(listing unavailable)") {
		t.Errorf("LIST failure should degrade to a note:\n%s", text)
	}
	if !strings.Contains(text, "## Outline") {
		t.Error("card should still render the rest")
	}
}

func TestHandlerMarkExplore_NonOKPassthrough(t *testing.T) {
	gs := graphstore.New()
	gs.ObserveDocument("mark://host/missing.md", graph.FetchResult{
		Status: "ok", Body: "# Old\n\n[target](/target.md)", Metadata: map[string]string{"version": "1"},
	})
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusNotFound,
				Metadata: map[string]string{"version": "2"},
			}}, nil
		},
	}
	h := &handler{client: sc, graphStore: gs}
	text := exploreText(t, h, "mark://host:6309/missing.md")
	if !strings.Contains(text, "status: not-found") {
		t.Errorf("non-ok fetch should pass through, got:\n%s", text)
	}
	if got := gs.Backlinks("mark://host/target.md"); len(got) != 0 {
		t.Fatalf("confirmed absence retained stale adjacency: %v", got)
	}
}

func TestHandlerMarkExplore_BinaryNotice(t *testing.T) {
	gs := graphstore.New()
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-07-04T00:00:00Z", "etag": "abc"},
				Body:     "[false relation](/false.md)\xff",
			}}, nil
		},
	}
	h := &handler{client: sc, graphStore: gs}
	text := exploreText(t, h, "mark://host:6309/img.png")
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text)
	}
	if strings.Contains(text, "## Outline") {
		t.Error("binary body must not be run through the outline builder")
	}
	if got := gs.Backlinks("mark://host/false.md"); len(got) != 0 {
		t.Fatalf("binary body cached false relation: %v", got)
	}
}

func TestHandlerMarkExplore_BinarySurfacesGraphSaveFailure(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "blocked")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	gs, err := graphstore.Load(filepath.Join(parent, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}, Body: "\x89PNG\xff",
			}}, nil
		},
	}
	h := &handler{client: sc, graphStore: gs}
	text := exploreText(t, h, "mark://host/image.png")
	if !strings.Contains(text, "graph cache save failed") {
		t.Fatalf("binary response hid cache failure:\n%s", text)
	}
}

func TestHandlerMarkExplore_InvalidURL(t *testing.T) {
	h := &handler{client: &stubClient{}}
	result, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{"url": "/bare"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}
