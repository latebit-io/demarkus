package main

import (
	"context"
	"fmt"
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

const exploreListing = `- [hub.md](hub.md)
- [alpha.md](alpha.md)
- [notes.md](notes.md)
- [docs/](docs/)
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
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://host:6309/hub.md", Title: "Hub", Status: "ok"})
	g.AddEdge("mark://host:6309/a.md", "mark://host:6309/hub.md")
	gs.Merge(g, nil)

	h := &handler{client: exploreStub(), graphStore: gs}
	text := exploreText(t, h, "mark://host:6309/hub.md")

	// The card shows node identity, which omits the default port (ADR 0005),
	// even though the caller addressed the document by its dial address.
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "[Page A](mark://host/a.md)") {
		t.Errorf("expected enriched backlink in card:\n%s", text)
	}
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
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusNotFound,
				Metadata: map[string]string{},
			}}, nil
		},
	}
	h := &handler{client: sc}
	text := exploreText(t, h, "mark://host:6309/missing.md")
	if !strings.Contains(text, "status: not-found") {
		t.Errorf("non-ok fetch should pass through, got:\n%s", text)
	}
}

func TestHandlerMarkExplore_BinaryNotice(t *testing.T) {
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "1", "modified": "2026-07-04T00:00:00Z", "etag": "abc"},
				Body:     "\x89PNG\r\n\x1a\n\xff\xfe\x00",
			}}, nil
		},
	}
	h := &handler{client: sc}
	text := exploreText(t, h, "mark://host:6309/img.png")
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text)
	}
	if strings.Contains(text, "## Outline") {
		t.Error("binary body must not be run through the outline builder")
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
