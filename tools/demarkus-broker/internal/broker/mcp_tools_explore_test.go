package broker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/protocol"
)

const exploreTestDoc = `# Hub

The hub links everything together.

## Sections

- [Alpha](/alpha.md)
- [Beta](/docs/beta.md)

## More

See [Alpha](/alpha.md) again (deduped).
`

const exploreTestListing = `- [alpha.md](alpha.md)
- [docs/](docs/)
- [hub.md](hub.md)
- [notes.md](notes.md)
`

func exploreDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "2", "modified": "2026-07-05T00:00:00Z", "etag": "xyz"},
				Body:     exploreTestDoc,
			}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   exploreTestListing,
			}}, nil
		},
	}
}

func exploreResultText(t *testing.T, g *mcpGateway, url string) string {
	t.Helper()
	res, err := g.handleMarkExplore(withAliceClaims(context.Background()), callToolReq("mark_explore", map[string]any{"url": url}))
	if err != nil {
		t.Fatalf("handleMarkExplore: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %+v", res.Content)
	}
	return toolResultText(t, res)
}

func TestHandleMarkExploreCard(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), exploreDispatcher())
	text := exploreResultText(t, g, "mark://team-a/hub.md")

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
		"## Backlinks (0)",
		"(none recorded; run mark_graph to populate",
		"## Siblings in / (3)",
		"- alpha.md",
		"- docs/",
		"fetch mark://team-a/hub.md#<anchor> for a section",
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

func TestHandleMarkExploreBacklinksFromGraphStore(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), exploreDispatcher())
	gr := graph.New()
	gr.AddNode(&graph.Node{URL: "mark://team-a/a.md", Title: "Page A", Status: "ok"})
	gr.AddNode(&graph.Node{URL: "mark://team-a/hub.md", Title: "Hub", Status: "ok"})
	gr.AddEdge("mark://team-a/a.md", "mark://team-a/hub.md")
	g.graphStore.Merge(gr, nil)

	text := exploreResultText(t, g, "mark://team-a/hub.md")
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "[Page A](mark://team-a/a.md)") {
		t.Errorf("expected enriched backlink in card:\n%s", text)
	}
}

func TestHandleMarkExploreSectionCaps(t *testing.T) {
	var body strings.Builder
	body.WriteString("# Big hub\n\n")
	for i := range 15 {
		fmt.Fprintf(&body, "## Section %d\n\n", i)
		fmt.Fprintf(&body, "- [Doc %d](/doc-%d.md)\n\n", i, i)
	}
	d := &fakeDispatcher{
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
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	text := exploreResultText(t, g, "mark://team-a/hub.md")

	if !strings.Contains(text, "+6 more headings") {
		t.Errorf("expected heading overflow marker (16 headings, cap 10):\n%s", text)
	}
	if !strings.Contains(text, "+5 more links") {
		t.Errorf("expected link overflow marker (15 links, cap 10):\n%s", text)
	}
}

func TestHandleMarkExploreListFailureDegrades(t *testing.T) {
	d := exploreDispatcher()
	d.listFn = func(_, _, _ string) (fetch.Result, error) {
		return fetch.Result{}, fmt.Errorf("boom")
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	text := exploreResultText(t, g, "mark://team-a/hub.md")
	if !strings.Contains(text, "(listing unavailable)") {
		t.Errorf("LIST failure should degrade to a note:\n%s", text)
	}
	if !strings.Contains(text, "## Outline") {
		t.Error("card should still render the rest")
	}
}

func TestHandleMarkExploreInvalidURL(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), &fakeDispatcher{})
	res, err := g.handleMarkExplore(withAliceClaims(context.Background()), callToolReq("mark_explore", map[string]any{
		"url": "mark:///no-world.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkExplore: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error for missing world name")
	}
}

func TestHandleMarkExploreBinaryNotice(t *testing.T) {
	g := newGatewayWithDispatcher(t, mcpTestConfig(), fetchModeDispatcher(binaryFetchModeDoc, "1", "abc"))
	text := exploreResultText(t, g, "mark://team-a/img.png")
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text)
	}
	if strings.Contains(text, "## Outline") {
		t.Error("binary body must not be run through the outline builder")
	}
}
