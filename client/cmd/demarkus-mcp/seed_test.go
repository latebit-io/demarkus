package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// hubGraphBody renders a /graph.md aggregate where a.md links to b.md.
func hubGraphBody() string {
	src := graphstore.New()
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Page A", Status: "ok", LinkCount: 1})
	g.AddNode(&graph.Node{URL: "mark://host:6309/b.md", Title: "Page B", Status: "ok"})
	g.AddEdgeInfo(graph.Edge{From: "mark://host:6309/a.md", To: "mark://host:6309/b.md", Label: "B", Count: 1})
	src.Merge(g, nil)
	return src.Export()
}

func emptyGraphStore(t *testing.T) *graphstore.Store {
	t.Helper()
	gs, err := graphstore.Load(filepath.Join(t.TempDir(), "graph.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return gs
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return text.Text
}

func TestSeedGraph_ColdStoreAnswersBacklinks(t *testing.T) {
	var gotPath string
	sc := &stubClient{
		fetchCondFn: func(_, path, _, _ string) (fetch.Result, error) {
			gotPath = path
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"etag": "hub-etag-1"},
				Body:     hubGraphBody(),
			}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "Page A") || !strings.Contains(text, "mark://host/a.md") {
		t.Errorf("seeded backlink missing from output: %s", text)
	}
	if gotPath != "/graph.md" {
		t.Errorf("seed fetched %q, want /graph.md", gotPath)
	}
	if got := h.graphStore.SeedEtag("host:6309"); got != "hub-etag-1" {
		t.Errorf("SeedEtag = %q, want hub-etag-1", got)
	}
}

// TestSeedGraph_DefaultHostWithoutPortCanonicalizes pins the live failure
// mode this feature shipped against: the -host flag typically omits the
// default port while the hub aggregate keys rows on mark://host:6309/...;
// the backlinks lookup must canonicalize or seeded rows never match.
func TestSeedGraph_DefaultHostWithoutPortCanonicalizes(t *testing.T) {
	sc := &stubClient{
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host", graphStore: emptyGraphStore(t)}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(t, res), "Page A") {
		t.Errorf("portless -host missed seeded canonical rows: %s", resultText(t, res))
	}
}

// TestSeedGraph_SeedsResolvedHostNotDefault: a full mark:// URL against a
// non-default host must seed that host's /graph.md (PR 253 review).
func TestSeedGraph_SeedsResolvedHostNotDefault(t *testing.T) {
	var gotHosts []string
	sc := &stubClient{
		fetchCondFn: func(host, _, _, _ string) (fetch.Result, error) {
			gotHosts = append(gotHosts, host)
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://elsewhere:6309", graphStore: emptyGraphStore(t)}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "mark://host:6309/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if len(gotHosts) != 1 || gotHosts[0] != "host:6309" {
		t.Errorf("seeded hosts = %v, want [host:6309] (the URL's host, not the default)", gotHosts)
	}
	if !strings.Contains(resultText(t, res), "Page A") {
		t.Errorf("backlinks missing from resolved-host seed: %s", resultText(t, res))
	}
}

func TestSeedGraph_NotFoundDegrades(t *testing.T) {
	sc := &stubClient{
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(t, res), "No backlinks found") {
		t.Errorf("expected empty-store hint, got: %s", resultText(t, res))
	}
}

func TestSeedGraph_FetchErrorDegrades(t *testing.T) {
	sc := &stubClient{
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			return fetch.Result{}, errors.New("dial refused")
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("seed failure must not fail the tool: %v", res.Content)
	}
	if !strings.Contains(resultText(t, res), "No backlinks found") {
		t.Errorf("expected empty-store hint, got: %s", resultText(t, res))
	}
}

func TestSeedGraph_LocalCrawlWins(t *testing.T) {
	gs := emptyGraphStore(t)
	// Local crawl observed a.md linking only to c.md.
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host:6309/a.md", Title: "Local A", Status: "ok", LinkCount: 1})
	g.AddEdgeInfo(graph.Edge{From: "mark://host:6309/a.md", To: "mark://host:6309/c.md"})
	gs.Merge(g, nil)

	sc := &stubClient{
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			// Hub still claims a.md links to b.md.
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: gs}

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(t, res), "No backlinks found") {
		t.Errorf("seed row for authoritative source must not win: %s", resultText(t, res))
	}

	res, err = h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/c.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(t, res), "Local A") {
		t.Errorf("local edge lost after seeding: %s", resultText(t, res))
	}
}

func TestSeedGraph_ThrottledWithinWindow(t *testing.T) {
	calls := 0
	sc := &stubClient{
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			calls++
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	for range 3 {
		if _, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"})); err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("seed fetches = %d, want 1 (throttled)", calls)
	}
}

func TestSeedGraph_EtagRoundTrip(t *testing.T) {
	var gotEtags []string
	sc := &stubClient{
		fetchCondFn: func(_, _, _, etag string) (fetch.Result, error) {
			gotEtags = append(gotEtags, etag)
			if etag == "hub-etag-1" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotModified}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"etag": "hub-etag-1"},
				Body:     hubGraphBody(),
			}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	if _, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"})); err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	// Expire the throttle so the second call re-checks with the stored etag.
	h.seedMu.Lock()
	h.seedChecked["host:6309"] = time.Now().Add(-2 * seedCheckInterval)
	h.seedMu.Unlock()

	res, err := h.markBacklinks(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if len(gotEtags) != 2 || gotEtags[0] != "" || gotEtags[1] != "hub-etag-1" {
		t.Errorf("etags sent = %v, want [\"\" hub-etag-1]", gotEtags)
	}
	// not-modified keeps the seeded rows.
	if !strings.Contains(resultText(t, res), "Page A") {
		t.Errorf("seeded backlink lost after not-modified refresh: %s", resultText(t, res))
	}
}

func TestSeedGraph_ExploreBacklinksSeeded(t *testing.T) {
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# B\n\nBody.\n"}}, nil
		},
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
		listFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: ""}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	res, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{"url": "/b.md"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "## Backlinks (1)") || !strings.Contains(text, "Page A") {
		t.Errorf("explore backlinks not seeded: %s", text)
	}
}

func TestSeedGraph_MarkGraphSeeds(t *testing.T) {
	calls := 0
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Doc\n"}}, nil
		},
		fetchCondFn: func(_, _, _, _ string) (fetch.Result, error) {
			calls++
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: hubGraphBody()}}, nil
		},
	}
	h := &handler{client: sc, defaultHost: "mark://host:6309", graphStore: emptyGraphStore(t)}

	if _, err := h.markGraph(context.Background(), newCallToolRequest(map[string]any{"url": "/doc.md", "depth": float64(1)})); err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if calls != 1 {
		t.Errorf("seed fetches during mark_graph = %d, want 1", calls)
	}
	// Hub context is present alongside the crawl result.
	if n := h.graphStore.GetNode("mark://host:6309/a.md"); n == nil {
		t.Error("seeded hub node missing after mark_graph")
	}
}
