package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

// worldGraphBody renders a /graph.md aggregate for team-a where a.md
// links to b.md.
func worldGraphBody() string {
	src := graphstore.New()
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://team-a/a.md", Title: "Page A", Status: "ok", LinkCount: 1})
	g.AddNode(&graph.Node{URL: "mark://team-a/b.md", Title: "Page B", Status: "ok"})
	g.AddEdgeInfo(graph.Edge{From: "mark://team-a/a.md", To: "mark://team-a/b.md", Label: "B", Count: 1})
	src.Merge(g, nil)
	return src.Export()
}

// seedingDispatcher scripts /graph.md on team-a with the given etag.
func seedingDispatcher(etag string) *fakeDispatcher {
	return &fakeDispatcher{
		fetchCondFn: func(_, path, _, sentEtag string) (fetch.Result, error) {
			if path != "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			if sentEtag != "" && sentEtag == etag {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotModified}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"etag": etag},
				Body:     worldGraphBody(),
			}}, nil
		},
	}
}

// TestHandleMarkBacklinksColdPodSeedsFromWorldGraph is the headline
// behavior: a cold gateway answers backlinks from the world's
// published /graph.md with no prior crawl.
func TestHandleMarkBacklinksColdPodSeedsFromWorldGraph(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if res.IsError {
		t.Fatalf("isError = true: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Page A") || !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("seeded backlink missing:\n%s", text)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.fetchCalls) != 0 {
		t.Errorf("plain fetches = %d, want 0 (no crawl needed)", len(d.fetchCalls))
	}
	if len(d.fetchCondCalls) != 1 || d.fetchCondCalls[0].path != "/graph.md" || d.fetchCondCalls[0].worldName != "team-a" {
		t.Errorf("fetchCondCalls = %+v, want one /graph.md check on team-a", d.fetchCondCalls)
	}
}

// TestHandleMarkGraphCrawlBeatsSeed: the seed runs before the crawl,
// and the crawl's view of a source replaces the seeded edges.
func TestHandleMarkGraphCrawlBeatsSeed(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	d.fetchFn = func(_, path, _ string) (fetch.Result, error) {
		if path == "/a.md" {
			// Locally, a.md now links to c.md, not b.md.
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   crawlBody("Page A", "/c.md"),
			}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: crawlBody("Doc")}}, nil
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	ctx := withAliceClaims(context.Background())

	if res, err := g.handleMarkGraph(ctx, callToolReq("mark_graph", map[string]any{
		"url": "mark://team-a/a.md",
	})); err != nil || res.IsError {
		t.Fatalf("mark_graph: err=%v text=%s", err, toolResultText(t, res))
	}

	res, err := g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "No backlinks") {
		t.Errorf("stale seeded edge survived the crawl:\n%s", text)
	}

	res, err = g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/c.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("crawled edge missing:\n%s", text)
	}
}

func TestSeedWorldGraphThrottledWithinWindow(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	ctx := withAliceClaims(context.Background())

	for range 3 {
		if _, err := g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
			"url": "mark://team-a/b.md",
		})); err != nil {
			t.Fatalf("handleMarkBacklinks: %v", err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.fetchCondCalls) != 1 {
		t.Errorf("seed checks = %d, want 1 (throttled)", len(d.fetchCondCalls))
	}
}

func TestSeedWorldGraphEtagRoundTrip(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	ctx := withAliceClaims(context.Background())

	if _, err := g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	})); err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}

	// Expire the throttle so the next call re-checks with the stored etag.
	g.graphSeedMu.Lock()
	g.graphSeedChecked["team-a"] = time.Now().Add(-2 * seedCheckInterval)
	g.graphSeedMu.Unlock()

	res, err := g.handleMarkBacklinks(ctx, callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}

	d.mu.Lock()
	calls := append([]condCall(nil), d.fetchCondCalls...)
	d.mu.Unlock()
	if len(calls) != 2 || calls[0].etag != "" || calls[1].etag != "hub-etag-1" {
		t.Errorf("etags sent = %+v, want [\"\" hub-etag-1]", calls)
	}
	// not-modified keeps the seeded rows.
	if text := toolResultText(t, res); !strings.Contains(text, "Page A") {
		t.Errorf("seeded backlink lost after not-modified refresh:\n%s", text)
	}
}

// TestHandleMarkExploreBacklinksSeeded: explore's backlinks section
// benefits from the same per-world seed.
func TestHandleMarkExploreBacklinksSeeded(t *testing.T) {
	d := seedingDispatcher("hub-etag-1")
	d.fetchFn = func(_, _, _ string) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# B\n\nBody.\n"}}, nil
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkExplore(withAliceClaims(context.Background()), callToolReq("mark_explore", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkExplore: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "Page A") {
		t.Errorf("explore backlinks not seeded:\n%s", text)
	}
}
