package broker

import (
	"context"
	"os"
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

// agentContractGolden is the /graph.md body the REAL federation agent
// produces for an in-cluster topology, generated and pinned by
// client/internal/fedcrawl's TestGraphExportContract. Consuming it here is
// the other half of the cross-module contract: producer drift regenerates
// the golden, and this suite fails if the seed path stops handling it.
const agentContractGoldenPath = "../../../../client/internal/fedcrawl/testdata/graph-export-incluster.md"

func agentContractGolden(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(agentContractGoldenPath)
	if err != nil {
		t.Fatalf("read agent export contract golden (regenerate with: go test ./internal/fedcrawl -run TestGraphExportContract -update, in client/): %v", err)
	}
	return string(body)
}

// TestSeedConsumesAgentExportContract: the real agent's export (the golden)
// must seed a cold gateway into answering world-name backlink queries, with
// every world-address row translated.
func TestSeedConsumesAgentExportContract(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{Name: "hub", Namespace: "hub", TokensSecret: "hub-tokens"})
	body := agentContractGolden(t)
	d := &fakeDispatcher{
		fetchCondFn: func(worldName, path, _, _ string) (fetch.Result, error) {
			if worldName == "hub" && path == "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/a.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	text := toolResultText(t, res)
	// index.md's plain link and b.md's typed rel edge, both translated,
	// with enriched provenance intact.
	for _, want := range []string{"mark://team-a/index.md", "mark://team-a/b.md", "[supersedes]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in backlinks from the agent's real export:\n%s", want, text)
		}
	}

	// Addressability invariant: every seeded mark:// row on a configured
	// world address must be translated; none may keep the dial-address form.
	for _, n := range g.graphStore.AllNodes() {
		if strings.Contains(n.URL, ".svc.cluster.local") {
			t.Errorf("unaddressable seeded row survived translation: %s", n.URL)
		}
	}
}

// internalAddrGraphBody renders a /graph.md the way the federation agent
// publishes it in-cluster: rows keyed by internal dial addresses
// (name.namespace.svc.cluster.local:6309), not world names.
func internalAddrGraphBody() string {
	src := graphstore.New()
	g := graph.New()
	const base = "mark://team-a.team-a.svc.cluster.local:6309"
	g.AddNode(&graph.Node{URL: base + "/a.md", Title: "Page A", Status: "ok", LinkCount: 2})
	g.AddNode(&graph.Node{URL: base + "/b.md", Title: "Page B", Status: "ok"})
	g.AddNode(&graph.Node{URL: "https://example.com/ext", Status: "external"})
	g.AddEdgeInfo(graph.Edge{From: base + "/a.md", To: base + "/b.md", Label: "B", Count: 1})
	g.AddEdgeInfo(graph.Edge{From: base + "/a.md", To: "https://example.com/ext", Count: 1})
	src.Merge(g, nil)
	return src.Export()
}

// TestSeedTranslatesInternalAddressesToWorldNames: the hub aggregate keys
// rows by cluster-internal DNS, which no legal tool URL can address; seeding
// must translate world addresses to world names and leave unknown hosts alone.
func TestSeedTranslatesInternalAddressesToWorldNames(t *testing.T) {
	d := &fakeDispatcher{
		fetchCondFn: func(_, path, _, _ string) (fetch.Result, error) {
			if path != "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: internalAddrGraphBody()}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "Page A") || !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("internal-address seed rows not translated to world names:\n%s", text)
	}
	// The external URL is not a world address and must stay as-is, on the
	// node and on the edge destination (asserted via the store; the tool
	// URL grammar only admits mark:// URLs).
	if n := g.graphStore.GetNode("https://example.com/ext"); n == nil {
		t.Error("external node URL was rewritten or dropped")
	}
	if bl := g.graphStore.Backlinks("https://example.com/ext"); len(bl) != 1 || bl[0] != "mark://team-a/a.md" {
		t.Errorf("external edge destination = %v, want [mark://team-a/a.md]", bl)
	}
}

// TestSeedFindsAggregateOnAnotherWorld: the aggregate lives only on the hub
// world, so a cold query against a different world must still seed it; every
// configured world is checked, not just the one in the query URL.
func TestSeedFindsAggregateOnAnotherWorld(t *testing.T) {
	cfg := mcpTestConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{Name: "hub", Namespace: "hub", TokensSecret: "hub-tokens"})
	d := &fakeDispatcher{
		fetchCondFn: func(worldName, path, _, _ string) (fetch.Result, error) {
			if worldName == "hub" && path == "/graph.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: internalAddrGraphBody()}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	g := newGatewayWithDispatcher(t, cfg, d)

	res, err := g.handleMarkBacklinks(withAliceClaims(context.Background()), callToolReq("mark_backlinks", map[string]any{
		"url": "mark://team-a/b.md",
	}))
	if err != nil {
		t.Fatalf("handleMarkBacklinks: %v", err)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "mark://team-a/a.md") {
		t.Errorf("hub-world aggregate not found from a team-a query:\n%s", text)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.fetchCondCalls) != 2 {
		t.Errorf("seed checks = %d, want 2 (every configured world once)", len(d.fetchCondCalls))
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
