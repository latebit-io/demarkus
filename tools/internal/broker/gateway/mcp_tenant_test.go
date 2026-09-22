package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/broker/storage"
	"github.com/mark3labs/mcp-go/mcp"
)

// The memory broker's authorization invariant: identity = world, reads
// AND writes locked to the caller's own world. This file enforces it
// across every registered tool, the resource surface, and the crawler.

func TestTenantWorldFor(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	alice := &core.Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}
	w, err := core.TenantWorldFor(cfg.Registry(), cfg.OIDC.Issuer, alice)
	if err != nil {
		t.Fatalf("tenantWorldFor(alice): %v", err)
	}
	if w.Name != "alice-w" {
		t.Errorf("tenant world = %q, want alice-w", w.Name)
	}

	stranger := &core.Claims{Subject: "google|eve", Email: "eve@example.com", EmailVerified: true}
	if _, err := core.TenantWorldFor(cfg.Registry(), cfg.OIDC.Issuer, stranger); !errors.Is(err, core.ErrNotAuthorized) {
		t.Errorf("tenantWorldFor(stranger) err = %v, want ErrNotAuthorized", err)
	}

	// Ambiguity denies closed; error text stays opaque (world names
	// identify tenants) while names ride the typed error for the log.
	cfg.Worlds[1].Allow.Emails = []string{"alice@example.com"}
	_, err = core.TenantWorldFor(cfg.Registry(), cfg.OIDC.Issuer, alice)
	if err == nil || errors.Is(err, core.ErrNotAuthorized) {
		t.Fatalf("tenantWorldFor(ambiguous) err = %v, want a distinct ambiguity error", err)
	}
	var ambiguous core.AmbiguousTenantError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("ambiguity err = %T, want errAmbiguousTenant", err)
	}
	if strings.Contains(err.Error(), "alice-w") || strings.Contains(err.Error(), "bob-w") {
		t.Errorf("ambiguity error leaks world names: %q", err.Error())
	}
	if ambiguous.First != "alice-w" || ambiguous.Second != "bob-w" {
		t.Errorf("ambiguity fields = %q/%q, want alice-w/bob-w", ambiguous.First, ambiguous.Second)
	}
}

func TestValidateTenantWorldsRejectsEmptyAllow(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	if err := cfg.ValidateTenantWorlds(); err != nil {
		t.Fatalf("ValidateTenantWorlds on valid config: %v", err)
	}
	cfg.Worlds[1].Allow = core.AllowConfig{}
	err := cfg.ValidateTenantWorlds()
	if err == nil {
		t.Fatal("ValidateTenantWorlds accepted a world with an empty allow block (would match every identity)")
	}
	if !strings.Contains(err.Error(), "bob-w") {
		t.Errorf("err = %v, want the offending world named", err)
	}
}

// TestMemoryProfileToolsAllClassified pins the registration invariant:
// every tool the memory profile ships must be classified in
// tenantWorldArgs, or tenantGate denies it closed.
func TestMemoryProfileToolsAllClassified(t *testing.T) {
	for _, tool := range MemoryProfile().Tools {
		if _, ok := tenantWorldArgs[tool.Name]; !ok {
			t.Errorf("memory profile tool %q is not classified in tenantWorldArgs; tenantGate would deny it", tool.Name)
		}
	}
}

// TestMemoryProfileExcludesFederationTools pins the surface: no
// multi-world aggregation, federation, or whole-store graph export.
func TestMemoryProfileExcludesFederationTools(t *testing.T) {
	excluded := []string{"mark_lookup_all", "mark_index", "mark_resolve", "mark_graph_export", "mark_graph_publish"}
	names := map[string]bool{}
	for _, tool := range MemoryProfile().Tools {
		names[tool.Name] = true
	}
	for _, name := range excluded {
		if names[name] {
			t.Errorf("memory profile must not expose %q", name)
		}
	}
	if !names["mark_worlds"] {
		t.Error("memory profile must keep mark_worlds (self-only) so clients can learn their world name")
	}
}

// TestTenantGateDeniesCrossTenantEveryTool is the invariant test: EVERY
// registered memory tool denies cross-tenant calls before any dispatch;
// iterating the profile means a future tool cannot ship unchecked.
func TestTenantGateDeniesCrossTenantEveryTool(t *testing.T) {
	for _, tool := range MemoryProfile().Tools {
		args, ok := tenantWorldArgs[tool.Name]
		if !ok || len(args) == 0 {
			continue // mark_worlds takes no URL; covered by its own test
		}
		t.Run(tool.Name, func(t *testing.T) {
			d := seededDispatcher()
			g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
			h := g.tenantGate(g.toolHandlers()[tool.Name])

			callArgs := map[string]any{"body": "x", "expected_version": float64(0)}
			for _, name := range args {
				callArgs[name] = "mark://bob-w/index.md"
			}
			res, err := h(withAliceClaims(context.Background()), callToolReq(tool.Name, callArgs))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("cross-tenant %s was not denied", tool.Name)
			}
			if text := toolResultText(t, res); !strings.Contains(text, "access denied") {
				t.Errorf("denial text = %q, want an access-denied message", text)
			}
			assertNoWorldTraffic(t, d, "bob-w")
		})
	}
}

// assertNoWorldTraffic fails if any recorded dispatch touched world.
func assertNoWorldTraffic(t *testing.T, d *fakeDispatcher, world string) {
	t.Helper()
	calls := d.Calls()
	touched := func(verb, host, path string) {
		if host == world {
			t.Errorf("%s dispatched to %s%s", verb, world, path)
		}
	}
	for _, c := range calls.Fetch {
		touched("fetch", c.Host, c.Path)
	}
	for _, c := range calls.Seed {
		touched("conditional fetch", c.Host, c.Path)
	}
	for _, c := range calls.List {
		touched("list", c.Host, c.Path)
	}
	for _, c := range calls.Versions {
		touched("versions", c.Host, c.Path)
	}
	for _, c := range calls.Archive {
		touched("archive", c.Host, c.Path)
	}
	for _, c := range calls.Lookup {
		touched("lookup", c.Host, c.Scope)
	}
	for _, c := range calls.Publish {
		touched("publish", c.Host, c.Path)
	}
	for _, c := range calls.Append {
		touched("append", c.Host, c.Path)
	}
}

// TestTenantGateDeniesUnclassifiedTool pins deny-closed dispatch: a
// tool name missing from tenantWorldArgs never reaches its handler.
func TestTenantGateDeniesUnclassifiedTool(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	called := false
	h := g.tenantGate(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_lookup_all", map[string]any{"query": "x"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unclassified tool was not denied")
	}
	if called {
		t.Fatal("unclassified tool reached its handler")
	}
}

func TestTenantGateDeniesForeignSections(t *testing.T) {
	for _, tool := range []string{"mark_fetch", "mark_explore"} {
		t.Run(tool, func(t *testing.T) {
			d := seededDispatcher()
			g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
			res, err := g.tenantGate(g.toolHandlers()[tool])(withAliceClaims(t.Context()), callToolReq(tool, map[string]any{"url": "mark://bob-w/index.md#memory"}))
			if err != nil || res == nil || !res.IsError {
				t.Fatalf("foreign section allowed: err=%v result=%v", err, res)
			}
			assertNoWorldTraffic(t, d, "bob-w")
		})
	}
}

// TestTenantGateDeniesUnknownIdentity: an authenticated identity with
// no provisioned world gets a denial, not a fallback world.
func TestTenantGateDeniesUnknownIdentity(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_fetch"])
	ctx := core.CtxWithClaims(context.Background(), &core.Claims{Subject: "google|eve", Email: "eve@example.com", EmailVerified: true})
	res, err := h(ctx, callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("identity with no world was not denied")
	}
	assertNoWorldTraffic(t, d, "alice-w")
	assertNoWorldTraffic(t, d, "bob-w")
}

func TestTenantGateAllowsOwnWorld(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_fetch"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("own-world fetch denied: %s", toolResultText(t, res))
	}
	assertNoWorldTraffic(t, d, "bob-w")
}

// TestMarkWorldsTenantScopedListsOnlyOwnWorld: the discovery tool must
// never reveal other tenants' world names (handleMarkWorlds is shared;
// tenant mode narrows it via scopedWorlds).
func TestMarkWorldsTenantScopedListsOnlyOwnWorld(t *testing.T) {
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), seededDispatcher())
	res, err := g.handleMarkWorlds(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "count: 1") {
		t.Errorf("mark_worlds output = %q, want count: 1", text)
	}
	if !strings.Contains(text, "alice-w") {
		t.Errorf("mark_worlds output missing the caller's world:\n%s", text)
	}
	if strings.Contains(text, "bob-w") {
		t.Errorf("mark_worlds leaked another tenant's world:\n%s", text)
	}
}

// TestMemoryResourceReadCrossTenantDenied: resource reads bypass the
// tool middleware, so readResource must enforce the same rule.
func TestMemoryResourceReadCrossTenantDenied(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	_, err := g.readResource(withAliceClaims(context.Background()), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "mark://bob-w/index.md"},
	})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("cross-tenant resource read err = %v, want access denied", err)
	}
	assertNoWorldTraffic(t, d, "bob-w")

	// Own world still reads.
	contents, err := g.readResource(withAliceClaims(context.Background()), mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "mark://alice-w/index.md"},
	})
	if err != nil {
		t.Fatalf("own-world resource read: %v", err)
	}
	if len(contents) == 0 {
		t.Fatal("own-world resource read returned no contents")
	}
}

// TestMemoryCrawlNeverLeavesTenantWorld: a document linking into
// another tenant's world must not cause the crawler to fetch it.
func TestMemoryCrawlNeverLeavesTenantWorld(t *testing.T) {
	d := seededDispatcher()
	d.Published["alice-w/notes.md"] = fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK,
		Body:   "# Notes\n\n[leak](mark://bob-w/secret.md)\n[own](mark://alice-w/index.md)\n",
	}}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_graph"])
	res, err := h(withAliceClaims(context.Background()), callToolReq("mark_graph", map[string]any{"url": "mark://alice-w/notes.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("own-world crawl denied: %s", toolResultText(t, res))
	}
	assertNoWorldTraffic(t, d, "bob-w")
}

func tenantGraphCall(t *testing.T, g *Gateway, tenant, tool, path string) string {
	t.Helper()
	req := callToolReq(tool, map[string]any{"url": "mark://" + tenant + "-w" + path})
	return tenantGraphRequest(t, g, tenant, &req)
}

func tenantGraphRequest(t *testing.T, g *Gateway, tenant string, req *mcp.CallToolRequest) string {
	t.Helper()
	ctx := core.CtxWithClaims(t.Context(), &core.Claims{Subject: "google|" + tenant, Email: tenant + "@example.com", EmailVerified: true})
	res, err := g.tenantGate(g.toolHandlers()[req.Params.Name])(ctx, *req)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("%s %s: err=%v result=%v", tenant, req.Params.Name, err, res)
	}
	return toolResultText(t, res)
}

func assertTenantBacklinks(t *testing.T, g *Gateway, tenant, foreign string) {
	t.Helper()
	for _, call := range []struct {
		tool      string
		relations bool
	}{{"mark_backlinks", false}, {"mark_explore", false}, {"mark_explore", true}} {
		tool := call.tool
		args := map[string]any{"url": "mark://" + tenant + "-w/index.md"}
		if call.relations {
			args["direction"], args["page_size"] = "both", 1
		}
		req := callToolReq(tool, args)
		text := tenantGraphRequest(t, g, tenant, &req)
		count := "Backlinks for mark://" + tenant + "-w/index.md (1)"
		if tool == "mark_explore" {
			count = "## Backlinks (1)"
		}
		if call.relations {
			count = "## Relations (1 documents)"
		}
		for _, want := range []string{tenant + "-w/source.md", tenant + "_TITLE", tenant + "_LABEL", count} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q:\n%s", tool, want, text)
			}
		}
		for _, secret := range []string{foreign + "-w/source.md", foreign + "_TITLE", foreign + "_LABEL", foreign + "_ANCHOR", foreign + "_anchor", "spoof"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaked %q:\n%s", tool, secret, text)
			}
		}
	}
}

func TestMemoryGraphSourcesIsolated(t *testing.T) {
	for _, order := range [][]string{{"alice", "bob"}, {"bob", "alice"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			d := seededDispatcher()
			for _, tenant := range order {
				d.Published[tenant+"-w/source.md"] = fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "# " + tenant + "_TITLE\n## " + tenant + "_ANCHOR\n[" + tenant + "_LABEL](mark://alice-w/index.md)\n[" + tenant + "_LABEL](mark://bob-w/index.md)\n",
				}}
			}
			g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
			for _, tenant := range order {
				tenantGraphCall(t, g, tenant, "mark_graph", "/source.md")
			}
			assertTenantBacklinks(t, g, "alice", "bob")
			assertTenantBacklinks(t, g, "bob", "alice")
		})
	}
}

func TestMemoryGraphSeedSourcesIsolated(t *testing.T) {
	for _, format := range []string{"legacy", "snapshot"} {
		t.Run(format, func(t *testing.T) { testMemoryGraphSeedSources(t, format) })
	}
}

func tenantSeedGraph(tenant string) *graphstore.Store {
	gr := graph.New()
	for _, owner := range []string{"alice", "bob"} {
		title := owner + "_TITLE"
		if owner != tenant {
			title = "spoof " + title
		}
		source := "mark://" + owner + "-w." + owner + "-w.svc.cluster.local:6309/source.md"
		observation := graph.Observe(source, map[string]string{"version": "1"})
		observation.Complete = true
		gr.AddNode(&graph.Node{URL: source, Title: title, Status: "ok", Observation: observation})
		for _, target := range []string{"alice", "bob"} {
			gr.AddEdgeInfo(graph.Edge{From: source, To: "mark://" + target + "-w/index.md", Rel: "related", Label: owner + "_LABEL", Anchor: owner + "_ANCHOR", Count: 7})
		}
	}
	store := graphstore.New()
	store.Merge(gr, nil)
	return store
}

func testMemoryGraphSeedSources(t *testing.T, format string) {
	t.Helper()
	for _, order := range [][]string{{"alice", "bob"}, {"bob", "alice"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			d := seededDispatcher()
			responses := make(map[string]protocol.Response)
			for _, tenant := range order {
				body := tenantSeedGraph(tenant).Export()
				if format == "legacy" {
					responses[tenant+"-w/graph.md"] = protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{"etag": "same-etag"}}
					continue
				}
				nodes, edges, err := graphstore.ParseExportStrict(body)
				if err != nil {
					t.Fatal(err)
				}
				manifest, shards := brokerSnapshotRows(t, nodes, edges)
				responses[tenant+"-w"+graphstore.SnapshotManifestPath] = manifest
				for path, shard := range shards {
					d.Published[tenant+"-w"+path] = fetch.Result{Response: shard}
				}
			}
			d.SeedFn = func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
				world, path := r.Host, r.Path
				response, ok := responses[world+path]
				if !ok {
					return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
				}
				return fetch.Result{Response: response}, nil
			}
			g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
			for _, tenant := range order {
				tenantGraphCall(t, g, tenant, "mark_backlinks", "/index.md")
			}
			assertTenantBacklinks(t, g, "alice", "bob")
			assertTenantBacklinks(t, g, "bob", "alice")
			for _, tenant := range order {
				text := tenantGraphCall(t, g, tenant, "mark_backlinks", "/index.md")
				for _, want := range []string{"[related]", "#" + tenant + "_ANCHOR", "x7"} {
					if !strings.Contains(text, want) {
						t.Errorf("own provenance missing %q: %s", want, text)
					}
				}
			}
		})
	}
}

func TestMemoryGraphScopeTransitions(t *testing.T) {
	for _, change := range []string{"identity", "address", "dial", "revoke"} {
		t.Run(change, func(t *testing.T) {
			cfg := brokertest.NewMemoryConfig()
			d := seededDispatcher()
			d.Published["alice-w/source.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# old secret\n[old](mark://alice-w/index.md)"}}
			g := newMemoryGateway(t, cfg, d)
			tenantGraphCall(t, g, "alice", "mark_graph", "/source.md")
			ctx := withAliceClaims(t.Context())
			old, err := g.graphFor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			old.graphStore.SetSeedEtag("alice-w", "old-etag")
			switch change {
			case "identity":
				ctx = core.CtxWithClaims(t.Context(), &core.Claims{Subject: "new-alice", Email: "alice@example.com", EmailVerified: true})
			case "address":
				cfg.Worlds[0].InternalAddress = "replacement:6309"
			case "dial":
				cfg.Worlds[0].DialAddress = "replacement:6310"
			case "revoke":
				cfg.Worlds[0].Allow.Emails = []string{"new-owner@example.com"}
			}
			for _, tool := range []string{"mark_backlinks", "mark_explore"} {
				res, err := g.tenantGate(g.toolHandlers()[tool])(ctx, callToolReq(tool, map[string]any{"url": "mark://alice-w/index.md"}))
				if err != nil || res == nil || res.IsError != (change == "revoke") {
					t.Fatalf("%s: err=%v result=%v", tool, err, res)
				}
				if text := toolResultText(t, res); strings.Contains(text, "source.md") || strings.Contains(text, "old secret") {
					t.Fatalf("stale graph after %s: %s", change, text)
				}
			}
			if change != "revoke" {
				current, err := g.graphFor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if current == old || current.graphStore.SeedEtag("alice-w") != "" {
					t.Fatal("scope change retained old graph or seed etag")
				}
			}
		})
	}
}

func TestMemoryGraphReauthorizationStartsFresh(t *testing.T) {
	for _, failure := range []string{"revoke", "deprovision"} {
		for _, entrypoint := range []string{"graph", "gate"} {
			t.Run(failure+"/"+entrypoint, func(t *testing.T) {
				testMemoryGraphReauthorization(t, failure, entrypoint)
			})
		}
	}
}

func testMemoryGraphReauthorization(t *testing.T, failure, entrypoint string) {
	t.Helper()
	cfg := brokertest.NewMemoryConfig()
	aliceWorld := cfg.Worlds[0]
	identities := map[string]string{core.IdentityKey(cfg.OIDC.Issuer, "google|alice"): aliceWorld.Name}
	if failure == "deprovision" {
		cfg.Worlds = cfg.Worlds[1:]
		cfg.Registry().SetDynamic([]core.WorldConfig{aliceWorld}, identities)
	}
	d := seededDispatcher()
	for _, tenant := range []string{"alice", "bob"} {
		d.Published[tenant+"-w/source.md"] = fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK, Body: "# " + tenant + "_TITLE\n[" + tenant + "_LABEL](mark://" + tenant + "-w/index.md)",
		}}
	}
	g := newMemoryGateway(t, cfg, d)
	tenantGraphCall(t, g, "alice", "mark_graph", "/source.md")
	tenantGraphCall(t, g, "bob", "mark_graph", "/source.md")
	ctx := withAliceClaims(t.Context())
	old, err := g.graphFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old.graphStore.SetSeedEtag("alice-w", "retired-etag")
	if failure == "deprovision" {
		cfg.Registry().SetDynamic(nil, nil)
	} else {
		cfg.Worlds[0].Allow.Emails = []string{"replacement@example.com"}
	}
	if entrypoint == "graph" {
		if _, err := g.graphFor(ctx); !errors.Is(err, core.ErrNotAuthorized) {
			t.Fatalf("graphFor error = %v, want ErrNotAuthorized", err)
		}
	} else {
		res, err := g.tenantGate(g.handleMarkFetch)(ctx, callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("gate did not deny access: err=%v result=%v", err, res)
		}
	}
	if _, retained := g.tenantGraphs["alice-w"]; retained {
		t.Error("authorization failure retained Alice's cached graph")
	}
	if failure == "deprovision" {
		cfg.Registry().SetDynamic([]core.WorldConfig{aliceWorld}, identities)
	} else {
		cfg.Worlds[0] = aliceWorld
	}
	current, err := g.graphFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current == old || current.graphStore.NodeCount() != 0 || current.graphStore.SeedEtag("alice-w") != "" || seedChecked(current, "alice-w") {
		t.Error("reauthorization reused retired graph data or seed bookkeeping")
	}
	for _, tool := range []string{"mark_backlinks", "mark_explore"} {
		if text := tenantGraphCall(t, g, "alice", tool, "/index.md"); strings.Contains(text, "alice-w/source.md") {
			t.Errorf("%s resurrected retired source: %s", tool, text)
		}
	}
	assertTenantBacklinks(t, g, "bob", "alice")
}

func TestMemoryGraphSeedRefreshIsScoped(t *testing.T) {
	d := seededDispatcher()
	empty := false
	d.SeedFn = func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		world, path, etag := r.Host, r.Path, r.IfNoneMatch
		if path != "/graph.md" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		}
		if empty && world == "alice-w" {
			if etag != "initial-etag" {
				t.Errorf("refresh etag = %q", etag)
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: graphstore.New().Export(), Metadata: map[string]string{"etag": "empty-etag"}}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: tenantSeedGraph(strings.TrimSuffix(world, "-w")).Export(), Metadata: map[string]string{"etag": "initial-etag"}}}, nil
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	assertTenantBacklinks(t, g, "alice", "bob")
	assertTenantBacklinks(t, g, "bob", "alice")
	state, err := g.graphFor(withAliceClaims(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	state.graphStore.ExpireSeedCheck("alice-w")
	empty = true
	for _, tool := range []string{"mark_backlinks", "mark_explore"} {
		if text := tenantGraphCall(t, g, "alice", tool, "/index.md"); strings.Contains(text, "source.md") {
			t.Fatalf("removed seed source survived: %s", text)
		}
	}
	assertTenantBacklinks(t, g, "bob", "alice")
	if state.graphStore.SeedEtag("alice-w") != "empty-etag" {
		t.Fatal("Alice seed did not refresh")
	}
}

func TestMemoryGraphRefreshDoesNotBlockAnotherTenant(t *testing.T) {
	d := seededDispatcher()
	started, release := make(chan struct{}), make(chan struct{})
	d.SeedFn = func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		world, path := r.Host, r.Path
		if world == "alice-w" && path == graphstore.SnapshotManifestPath {
			close(started)
			<-release
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	finished := make(chan struct{})
	var aliceResult *mcp.CallToolResult
	var aliceErr error
	go func() {
		defer close(finished)
		aliceResult, aliceErr = g.tenantGate(g.handleMarkBacklinks)(withAliceClaims(t.Context()), callToolReq("mark_backlinks", map[string]any{"url": "mark://alice-w/index.md"}))
	}()
	defer func() {
		close(release)
		<-finished
		if aliceErr != nil || aliceResult == nil || aliceResult.IsError {
			t.Errorf("Alice query failed: err=%v result=%v", aliceErr, aliceResult)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Alice seed did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx = core.CtxWithClaims(ctx, &core.Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true})
	res, err := g.tenantGate(g.handleMarkBacklinks)(ctx, callToolReq("mark_backlinks", map[string]any{"url": "mark://bob-w/index.md"}))
	if err != nil || res == nil || res.IsError || ctx.Err() != nil {
		t.Fatalf("Bob waited for Alice seed: err=%v result=%v context=%v", err, res, ctx.Err())
	}
	if _, ok := g.tenantGraphs["bob-w"]; !ok {
		t.Fatal("Bob graph was not initialized independently")
	}
}

func TestMemoryGraphRetiredRefreshCannotPopulateNewScope(t *testing.T) {
	d := seededDispatcher()
	d.Published["alice-w/source.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}
	manifest, shards := brokerSnapshotRows(t,
		[]graphstore.StoredNode{{URL: "mark://alice-w/source.md", Title: "old private source", Status: "ok"}},
		[]graphstore.StoredEdge{{From: "mark://alice-w/source.md", To: "mark://alice-w/index.md", Count: 1}})
	for path, shard := range shards {
		d.Published["alice-w"+path] = fetch.Result{Response: shard}
	}
	started, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	d.SeedFn = func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		path := r.Path
		if path == graphstore.SnapshotManifestPath && !first.Swap(true) {
			close(started)
			<-release
			return fetch.Result{Response: manifest}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), d)
	handler := g.tenantGate(g.handleMarkBacklinks)
	req := callToolReq("mark_backlinks", map[string]any{"url": "mark://alice-w/index.md"})
	finished := make(chan struct{})
	var oldResult *mcp.CallToolResult
	var oldErr error
	go func() {
		defer close(finished)
		oldResult, oldErr = handler(withAliceClaims(t.Context()), req)
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
		<-finished
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old seed did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx = core.CtxWithClaims(ctx, &core.Claims{Subject: "replacement-alice", Email: "alice@example.com", EmailVerified: true})
	res, err := handler(ctx, req)
	if err != nil || res == nil || res.IsError || ctx.Err() != nil {
		t.Fatalf("new owner waited on retired seed: err=%v result=%v context=%v", err, res, ctx.Err())
	}
	close(release)
	released = true
	<-finished
	if oldErr != nil || oldResult == nil || oldResult.IsError || !strings.Contains(toolResultText(t, oldResult), "old private source") {
		t.Fatalf("old seed did not finish: err=%v result=%v", oldErr, oldResult)
	}
	res, err = handler(ctx, req)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("new owner query: err=%v result=%v", err, res)
	}
	if text := toolResultText(t, res); strings.Contains(text, "source.md") || strings.Contains(text, "old private") {
		t.Fatalf("late retired seed contaminated replacement scope: %s", text)
	}
}

// TestMemoryGatewayEndToEnd drives the full Streamable HTTP transport:
// initialize carries instructions plus the server name, tools/list is
// exactly the memory surface, and a cross-tenant call is denied at the wire.
func TestMemoryGatewayEndToEnd(t *testing.T) {
	ts := newGatewayFixture(t, brokertest.NewMemoryConfig(), &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, MemoryProfile()).serve(t, seededDispatcher())

	resp := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if resp.HTTPStatus != http.StatusOK || resp.Error != nil {
		t.Fatalf("initialize: status=%d err=%+v body=%s", resp.HTTPStatus, resp.Error, resp.RawBody)
	}
	if info, ok := resp.Result["serverInfo"].(map[string]any); !ok || info["name"] != "demarkus-memory-broker" {
		t.Errorf("serverInfo = %+v, want name demarkus-memory-broker", resp.Result["serverInfo"])
	}
	instructions, _ := resp.Result["instructions"].(string)
	if !strings.Contains(instructions, "memory") {
		t.Errorf("initialize instructions missing memory guidance: %q", instructions)
	}
	session := resp.Headers[mcpSessionHeader]

	listResp := mcpRequest(t, ts.URL, "alice-token", session, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	if listResp.Error != nil {
		t.Fatalf("tools/list error: %+v", listResp.Error)
	}
	rawTools, err := json.Marshal(listResp.Result["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range tools {
		got[tool.Name] = true
	}
	want := MemoryProfile().Tools
	if len(tools) != len(want) {
		t.Errorf("tools/list returned %d tools, want %d", len(tools), len(want))
	}
	for _, tool := range want {
		if !got[tool.Name] {
			t.Errorf("tools/list missing %q", tool.Name)
		}
	}

	callResp := mcpRequest(t, ts.URL, "alice-token", session, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "mark_fetch",
			"arguments": map[string]any{"url": "mark://bob-w/index.md"},
		},
	})
	if callResp.Error != nil {
		t.Fatalf("tools/call transport error: %+v", callResp.Error)
	}
	if isErr, _ := callResp.Result["isError"].(bool); !isErr {
		t.Fatalf("cross-tenant mark_fetch over the wire was not denied: %+v", callResp.Result)
	}
}

// TestTenantGateProvisionsFirstArrival: an admitted identity with no
// world gets one created on its first tool call, and the created world
// then serves it while other tenants stay unreachable.
func TestTenantGateProvisionsFirstArrival(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	eveCtx := core.CtxWithClaims(context.Background(), brokertest.EveClaims())

	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	res, err := h(eveCtx, callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("first arrival not provisioned: %s", toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "eve-adams-") {
		t.Errorf("mark_worlds after provisioning = %q, want the new slug", text)
	}
	if len(buckets.Created) != 1 {
		t.Errorf("buckets created = %v, want exactly one", buckets.Created)
	}

	// Cross-tenant stays denied for the new tenant too.
	fetchGate := g.tenantGate(g.toolHandlers()["mark_fetch"])
	res, err = fetchGate(eveCtx, callToolReq("mark_fetch", map[string]any{"url": "mark://alice-w/index.md"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("provisioned tenant reached another tenant's world")
	}
}

// TestTenantGateEmailChangeResolvesSameWorld: after an email change the
// gate serves the ORIGINAL world (one slow-path refresh, then the
// resolver fast path with no further EnsureTenant).
func TestTenantGateEmailChangeResolvesSameWorld(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])

	res, err := h(core.CtxWithClaims(context.Background(), brokertest.EveClaims()), callToolReq("mark_worlds", nil))
	if err != nil || res.IsError {
		t.Fatalf("first arrival: err=%v res=%s", err, toolResultText(t, res))
	}
	slug := storage.TenantSlug(cfg.OIDC.Issuer, "google|eve-123", "eve.adams@example.com")

	moved := &core.Claims{Subject: "google|eve-123", Email: "Eve.Moved@elsewhere.io", EmailVerified: true}
	res, err = h(core.CtxWithClaims(context.Background(), moved), callToolReq("mark_worlds", nil))
	if err != nil || res.IsError {
		t.Fatalf("post-change call: err=%v res=%s", err, toolResultText(t, res))
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, slug) {
		t.Errorf("mark_worlds after email change = %q, want the original slug %q", text, slug)
	}
	if strings.Contains(text, "eve-moved-") {
		t.Errorf("email change provisioned a second world: %q", text)
	}

	// Third call must ride the resolver fast path: the refresh already
	// converged, so EnsureTenant (and its bucket re-ensure) stays idle.
	ensured := len(buckets.Created)
	res, err = h(core.CtxWithClaims(context.Background(), moved), callToolReq("mark_worlds", nil))
	if err != nil || res.IsError {
		t.Fatalf("fast-path call: err=%v res=%s", err, toolResultText(t, res))
	}
	if len(buckets.Created) != ensured {
		t.Errorf("fast path re-ran EnsureTenant: bucket ensures %d -> %d", ensured, len(buckets.Created))
	}
}

// TestTenantGateProvisioningNotReadyMessage: when the backend has not
// picked the new world up yet, the caller gets a clear retry message
// instead of a transport error.
func TestTenantGateProvisioningNotReadyMessage(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	d := &fakeDispatcher{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			worldName := r.Host
			if strings.HasPrefix(worldName, "eve-adams-") {
				return fetch.Result{}, errors.New("dial: no route to world")
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
		},
	}
	g, _ := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	res, err := h(core.CtxWithClaims(context.Background(), brokertest.EveClaims()), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("not-ready world did not produce a retry message")
	}
	if text := toolResultText(t, res); !strings.Contains(text, "being provisioned") {
		t.Errorf("text = %q, want provisioning-in-progress message", text)
	}
}

// TestTenantGateDeniedIdentityGetsNoWorld: provisioning enabled but the
// gate rejects the identity; nothing is created.
func TestTenantGateDeniedIdentityGetsNoWorld(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionAllowlisted)
	cfg.Provisioning.Allow = core.AllowConfig{Domains: []string{"other.org"}}
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	res, err := h(core.CtxWithClaims(context.Background(), brokertest.EveClaims()), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("denied identity was provisioned")
	}
	if len(buckets.Created) != 0 {
		t.Errorf("denied identity created buckets: %v", buckets.Created)
	}
}

func TestMemoryGraphScopeRetiresIdleTenants(t *testing.T) {
	f := newGatewayFixture(t, brokertest.NewMemoryConfig(), &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, MemoryProfile())
	g := f.gateway(seededDispatcher())
	alice := withAliceClaims(t.Context())
	bob := core.CtxWithClaims(t.Context(), &core.Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true})

	first, err := g.graphFor(alice)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(tenantGraphTTL / 2)
	if again, err := g.graphFor(alice); err != nil || again != first {
		t.Fatalf("scope within TTL must persist: err=%v same=%v", err, again == first)
	}
	f.clock.Advance(tenantGraphTTL + time.Minute)
	if _, err := g.graphFor(bob); err != nil {
		t.Fatal(err)
	}
	if _, retained := g.tenantGraphs["alice-w"]; retained {
		t.Fatal("idle scope survived another tenant's call past the TTL")
	}
	if replaced, err := g.graphFor(alice); err != nil || replaced == first {
		t.Fatalf("retired scope must re-seed fresh: err=%v same=%v", err, replaced == first)
	}
}

func TestMemoryGraphScopeCapsTenants(t *testing.T) {
	g := newMemoryGateway(t, brokertest.NewMemoryConfig(), seededDispatcher())
	now := fixedTestTime
	for i := range maxTenantGraphs {
		g.tenantGraphs[fmt.Sprintf("w%d", i)] = &tenantGraph{
			graph: &gatewayGraph{graphStore: graphstore.New()}, lastUsed: now.Add(time.Duration(i) * time.Second),
		}
	}
	if _, err := g.graphFor(withAliceClaims(t.Context())); err != nil {
		t.Fatal(err)
	}
	if got := len(g.tenantGraphs); got != maxTenantGraphs {
		t.Fatalf("tenant graphs = %d, want cap %d", got, maxTenantGraphs)
	}
	if _, evicted := g.tenantGraphs["w0"]; evicted {
		t.Fatal("least recently used scope survived the cap")
	}
	if _, kept := g.tenantGraphs["alice-w"]; !kept {
		t.Fatal("new scope missing after eviction")
	}
}

func seedChecked(state *gatewayGraph, world string) bool {
	_, checked := state.graphStore.LastSeedCheck(world)
	return checked
}

// A first read through the resource picker is a first arrival like any tool
// call: the world is provisioned, and another tenant's stays out of reach.
func TestResourceReadProvisionsFirstArrival(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	eveCtx := core.CtxWithClaims(context.Background(), brokertest.EveClaims())

	_, err := g.readResource(eveCtx, mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "mark://alice-w/index.md"}})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("cross-tenant read err = %v, want access denied", err)
	}
	if len(buckets.Created) != 1 {
		t.Fatalf("buckets created = %v, want the first arrival provisioned", buckets.Created)
	}
	worlds := g.scopedWorlds(eveCtx)
	if len(worlds) != 1 || !strings.HasPrefix(worlds[0].Name, "eve-adams-") {
		t.Errorf("worlds = %+v, want eve's own", worlds)
	}
}

// countingStore counts reads of the broker's Secrets.
type countingStore struct {
	core.SecretStore
	mutates int
}

func (c *countingStore) Mutate(ctx context.Context, ref core.SecretRef, mutate func([]byte) ([]byte, error)) error {
	c.mutates++
	return c.SecretStore.Mutate(ctx, ref, mutate)
}

// An identity the service does not admit is told so once a minute, not by a
// registry round trip under the provisioner's lock on every call.
func TestDeniedIdentityIsRememberedBriefly(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionAllowlisted)
	cfg.Provisioning.Allow = core.AllowConfig{Domains: []string{"other.org"}}
	f := newGatewayFixture(t, cfg, &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}, MemoryProfile())
	store := &countingStore{SecretStore: f.store}
	f.store = store
	f.enableProvisioning(&brokertest.FakeBuckets{})
	f.clock.Set(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	g := f.gateway(seededDispatcher())
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	call := func() string {
		res, err := h(core.CtxWithClaims(context.Background(), brokertest.EveClaims()), callToolReq("mark_worlds", nil))
		if err != nil || !res.IsError {
			t.Fatalf("gate = (%+v, %v), want a denial", res, err)
		}
		return toolResultText(t, res)
	}
	first := call()
	if second := call(); second != first || store.mutates != 1 {
		t.Errorf("second = %q (first %q), registry reads = %d, want the same answer from memory", second, first, store.mutates)
	}
	f.clock.Advance(2 * time.Minute)
	call()
	if store.mutates != 2 {
		t.Errorf("registry reads = %d, want a fresh check after the denial expired", store.mutates)
	}
}

// A tool the gate does not know is refused before anything is provisioned.
func TestTenantGateRefusesUnclassifiedToolBeforeProvisioning(t *testing.T) {
	g, buckets := memoryGatewayWithProvisioning(t, brokertest.NewProvisioningConfig(core.ProvisionOpen), seededDispatcher())
	h := g.tenantGate(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Fatal("an unclassified tool reached its handler")
		return nil, nil
	})
	res, err := h(core.CtxWithClaims(context.Background(), brokertest.EveClaims()), callToolReq("mark_mystery", nil))
	if err != nil || !res.IsError || !strings.Contains(toolResultText(t, res), "not available") {
		t.Fatalf("gate = (%+v, %v)", res, err)
	}
	if len(buckets.Created) != 0 {
		t.Errorf("buckets created = %v for a tool that does not exist", buckets.Created)
	}
}
