package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

// The memory broker's authorization invariant: identity = world, reads
// AND writes locked to the caller's own world. This file enforces it
// across every registered tool, the resource surface, and the crawler.

// memoryTestConfig provisions two static tenants: alice owns alice-w,
// bob owns bob-w.
func memoryTestConfig() *Config {
	cfg := testConfig()
	cfg.Server.MCP = MCPConfig{Addr: ":0", PublicURL: "https://memory.example.com"}
	cfg.Worlds = []WorldConfig{
		{
			Name: "alice-w", Namespace: "alice-w", TokensSecret: "alice-w-tokens",
			Allow: AllowConfig{Emails: []string{"alice@example.com"}},
		},
		{
			Name: "bob-w", Namespace: "bob-w", TokensSecret: "bob-w-tokens",
			Allow: AllowConfig{Emails: []string{"bob@example.com"}},
		},
	}
	return cfg
}

// newMemoryGateway builds a memory-profile gateway around a fake
// dispatcher, mirroring newGatewayWithDispatcher.
func newMemoryGateway(t testing.TB, cfg *Config, d worldDispatcher) *mcpGateway {
	t.Helper()
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	srv.clock = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return newMCPGateway(srv, "test", d, MemoryGatewayProfile())
}

// seededDispatcher returns a fakeDispatcher whose worlds already carry
// /index.md, so tenantGate's memory seeding takes the already-seeded fast
// path and tests do not see seed publishes unless they want them.
func seededDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		published: map[string]fetch.Result{
			"alice-w/index.md": {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Memory"}},
			"bob-w/index.md":   {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Memory"}},
		},
	}
}

func TestTenantWorldFor(t *testing.T) {
	cfg := memoryTestConfig()
	alice := &Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}
	w, err := tenantWorldFor(cfg, alice)
	if err != nil {
		t.Fatalf("tenantWorldFor(alice): %v", err)
	}
	if w.Name != "alice-w" {
		t.Errorf("tenant world = %q, want alice-w", w.Name)
	}

	stranger := &Claims{Subject: "google|eve", Email: "eve@example.com", EmailVerified: true}
	if _, err := tenantWorldFor(cfg, stranger); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("tenantWorldFor(stranger) err = %v, want ErrNotAuthorized", err)
	}

	// Ambiguity denies closed; error text stays opaque (world names
	// identify tenants) while names ride the typed error for the log.
	cfg.Worlds[1].Allow.Emails = []string{"alice@example.com"}
	_, err = tenantWorldFor(cfg, alice)
	if err == nil || errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("tenantWorldFor(ambiguous) err = %v, want a distinct ambiguity error", err)
	}
	var ambiguous errAmbiguousTenant
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
	cfg := memoryTestConfig()
	if err := cfg.ValidateTenantWorlds(); err != nil {
		t.Fatalf("ValidateTenantWorlds on valid config: %v", err)
	}
	cfg.Worlds[1].Allow = AllowConfig{}
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
	for _, tool := range MemoryGatewayProfile().Tools {
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
	for _, tool := range MemoryGatewayProfile().Tools {
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
	for _, tool := range MemoryGatewayProfile().Tools {
		args, ok := tenantWorldArgs[tool.Name]
		if !ok || len(args) == 0 {
			continue // mark_worlds takes no URL; covered by its own test
		}
		t.Run(tool.Name, func(t *testing.T) {
			d := seededDispatcher()
			g := newMemoryGateway(t, memoryTestConfig(), d)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.fetchCalls {
		if c.worldName == world {
			t.Errorf("fetch dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.fetchCondCalls {
		if c.worldName == world {
			t.Errorf("conditional fetch dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.listCalls {
		if c.worldName == world {
			t.Errorf("list dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.versionsCalls {
		if c.worldName == world {
			t.Errorf("versions dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.lookupCalls {
		if c.worldName == world {
			t.Errorf("lookup dispatched to %s", world)
		}
	}
	for _, c := range d.publishCalls {
		if c.worldName == world {
			t.Errorf("publish dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.appendCalls {
		if c.worldName == world {
			t.Errorf("append dispatched to %s%s", world, c.path)
		}
	}
	for _, c := range d.archiveCalls {
		if c.worldName == world {
			t.Errorf("archive dispatched to %s%s", world, c.path)
		}
	}
}

// TestTenantGateDeniesUnclassifiedTool pins deny-closed dispatch: a
// tool name missing from tenantWorldArgs never reaches its handler.
func TestTenantGateDeniesUnclassifiedTool(t *testing.T) {
	d := seededDispatcher()
	g := newMemoryGateway(t, memoryTestConfig(), d)
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
			g := newMemoryGateway(t, memoryTestConfig(), d)
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
	g := newMemoryGateway(t, memoryTestConfig(), d)
	h := g.tenantGate(g.toolHandlers()["mark_fetch"])
	ctx := ctxWithClaims(context.Background(), &Claims{Subject: "google|eve", Email: "eve@example.com", EmailVerified: true})
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
	g := newMemoryGateway(t, memoryTestConfig(), d)
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
	g := newMemoryGateway(t, memoryTestConfig(), seededDispatcher())
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
	g := newMemoryGateway(t, memoryTestConfig(), d)
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
	d.published["alice-w/notes.md"] = fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK,
		Body:   "# Notes\n\n[leak](mark://bob-w/secret.md)\n[own](mark://alice-w/index.md)\n",
	}}
	g := newMemoryGateway(t, memoryTestConfig(), d)
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

func tenantGraphCall(t *testing.T, g *mcpGateway, tenant, tool, path string) string {
	t.Helper()
	ctx := ctxWithClaims(t.Context(), &Claims{Subject: "google|" + tenant, Email: tenant + "@example.com", EmailVerified: true})
	res, err := g.tenantGate(g.toolHandlers()[tool])(ctx, callToolReq(tool, map[string]any{"url": "mark://" + tenant + "-w" + path}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("%s %s: err=%v result=%v", tenant, tool, err, res)
	}
	return toolResultText(t, res)
}

func assertTenantBacklinks(t *testing.T, g *mcpGateway, tenant, foreign string) {
	t.Helper()
	for _, tool := range []string{"mark_backlinks", "mark_explore"} {
		text := tenantGraphCall(t, g, tenant, tool, "/index.md")
		count := "Backlinks for mark://" + tenant + "-w/index.md (1)"
		if tool == "mark_explore" {
			count = "## Backlinks (1)"
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
				d.published[tenant+"-w/source.md"] = fetch.Result{Response: protocol.Response{
					Status: protocol.StatusOK,
					Body:   "# " + tenant + "_TITLE\n## " + tenant + "_ANCHOR\n[" + tenant + "_LABEL](mark://alice-w/index.md)\n[" + tenant + "_LABEL](mark://bob-w/index.md)\n",
				}}
			}
			g := newMemoryGateway(t, memoryTestConfig(), d)
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
		gr.AddNode(&graph.Node{URL: source, Title: title, Status: "ok"})
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
					d.published[tenant+"-w"+path] = fetch.Result{Response: shard}
				}
			}
			d.fetchCondFn = func(world, path, _, _ string) (fetch.Result, error) {
				response, ok := responses[world+path]
				if !ok {
					return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
				}
				return fetch.Result{Response: response}, nil
			}
			g := newMemoryGateway(t, memoryTestConfig(), d)
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
			cfg := memoryTestConfig()
			d := seededDispatcher()
			d.published["alice-w/source.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# old secret\n[old](mark://alice-w/index.md)"}}
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
				ctx = ctxWithClaims(t.Context(), &Claims{Subject: "new-alice", Email: "alice@example.com", EmailVerified: true})
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

func TestMemoryGraphSeedRefreshIsScoped(t *testing.T) {
	d := seededDispatcher()
	empty := false
	d.fetchCondFn = func(world, path, _, etag string) (fetch.Result, error) {
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
	g := newMemoryGateway(t, memoryTestConfig(), d)
	assertTenantBacklinks(t, g, "alice", "bob")
	assertTenantBacklinks(t, g, "bob", "alice")
	state, err := g.graphFor(withAliceClaims(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	state.graphSeedMu.Lock()
	state.graphSeedChecked["alice-w"] = time.Now().Add(-2 * seedCheckInterval)
	state.graphSeedMu.Unlock()
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
	d.fetchCondFn = func(world, path, _, _ string) (fetch.Result, error) {
		if world == "alice-w" && path == graphstore.SnapshotManifestPath {
			close(started)
			<-release
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	g := newMemoryGateway(t, memoryTestConfig(), d)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tenantGraphCall(t, g, "alice", "mark_backlinks", "/index.md")
	}()
	defer func() { close(release); <-finished }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Alice seed did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx = ctxWithClaims(ctx, &Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true})
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
	manifest, shards := brokerSnapshotRows(t,
		[]graphstore.StoredNode{{URL: "mark://alice-w/source.md", Title: "old private source", Status: "ok"}},
		[]graphstore.StoredEdge{{From: "mark://alice-w/source.md", To: "mark://alice-w/index.md", Count: 1}})
	for path, shard := range shards {
		d.published["alice-w"+path] = fetch.Result{Response: shard}
	}
	started, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	d.fetchCondFn = func(_, path, _, _ string) (fetch.Result, error) {
		if path == graphstore.SnapshotManifestPath && !first.Swap(true) {
			close(started)
			<-release
			return fetch.Result{Response: manifest}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	g := newMemoryGateway(t, memoryTestConfig(), d)
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
	ctx = ctxWithClaims(ctx, &Claims{Subject: "replacement-alice", Email: "alice@example.com", EmailVerified: true})
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
	cfg := memoryTestConfig()
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	ts := httptest.NewServer(srv.MCPGatewayWith("test", seededDispatcher(), MemoryGatewayProfile()))
	t.Cleanup(ts.Close)

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
	want := MemoryGatewayProfile().Tools
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

// memoryGatewayWithProvisioning wires a memory gateway whose server has
// dynamic provisioning enabled over the k8s fake Secret store.
func memoryGatewayWithProvisioning(t *testing.T, cfg *Config, d worldDispatcher) (*mcpGateway, *fakeBuckets) {
	t.Helper()
	g := newMemoryGateway(t, cfg, d)
	buckets := &fakeBuckets{}
	g.srv.EnableProvisioning(buckets)
	return g, buckets
}

// TestTenantGateProvisionsFirstArrival: an admitted identity with no
// world gets one created on its first tool call, and the created world
// then serves it while other tenants stay unreachable.
func TestTenantGateProvisionsFirstArrival(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	eveCtx := ctxWithClaims(context.Background(), eveClaims())

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
	if len(buckets.created) != 1 {
		t.Errorf("buckets created = %v, want exactly one", buckets.created)
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
	cfg := provisioningTestConfig(ProvisionOpen)
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])

	res, err := h(ctxWithClaims(context.Background(), eveClaims()), callToolReq("mark_worlds", nil))
	if err != nil || res.IsError {
		t.Fatalf("first arrival: err=%v res=%s", err, toolResultText(t, res))
	}
	slug := tenantSlug(cfg.OIDC.Issuer, "google|eve-123", "eve.adams@example.com")

	moved := &Claims{Subject: "google|eve-123", Email: "Eve.Moved@elsewhere.io", EmailVerified: true}
	res, err = h(ctxWithClaims(context.Background(), moved), callToolReq("mark_worlds", nil))
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
	ensured := len(buckets.created)
	res, err = h(ctxWithClaims(context.Background(), moved), callToolReq("mark_worlds", nil))
	if err != nil || res.IsError {
		t.Fatalf("fast-path call: err=%v res=%s", err, toolResultText(t, res))
	}
	if len(buckets.created) != ensured {
		t.Errorf("fast path re-ran EnsureTenant: bucket ensures %d -> %d", ensured, len(buckets.created))
	}
}

// TestTenantGateProvisioningNotReadyMessage: when the backend has not
// picked the new world up yet, the caller gets a clear retry message
// instead of a transport error.
func TestTenantGateProvisioningNotReadyMessage(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	d := &fakeDispatcher{
		fetchFn: func(worldName, _, _ string) (fetch.Result, error) {
			if strings.HasPrefix(worldName, "eve-adams-") {
				return fetch.Result{}, errors.New("dial: no route to world")
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
		},
	}
	g, _ := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	res, err := h(ctxWithClaims(context.Background(), eveClaims()), callToolReq("mark_worlds", nil))
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
	cfg := provisioningTestConfig(ProvisionAllowlisted)
	cfg.Provisioning.Allow = AllowConfig{Domains: []string{"other.org"}}
	d := seededDispatcher()
	g, buckets := memoryGatewayWithProvisioning(t, cfg, d)
	h := g.tenantGate(g.toolHandlers()["mark_worlds"])
	res, err := h(ctxWithClaims(context.Background(), eveClaims()), callToolReq("mark_worlds", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("denied identity was provisioned")
	}
	if len(buckets.created) != 0 {
		t.Errorf("denied identity created buckets: %v", buckets.created)
	}
}

// TestInstallableWorldsTenantScoped: the management API's world listing
// must not leak other tenants' world names on the memory broker.
func TestInstallableWorldsTenantScoped(t *testing.T) {
	cfg := memoryTestConfig()
	cfg.Worlds[0].PublicURL = "mark://alice-w.example:6309"
	cfg.Worlds[1].PublicURL = "mark://bob-w.example:6309"
	alice := &Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}

	scoped, err := installableWorlds(cfg, alice, true)
	if err != nil {
		t.Fatalf("tenant-scoped listing: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Name != "alice-w" {
		t.Errorf("tenant-scoped install worlds = %+v, want only alice-w", scoped)
	}
	open, err := installableWorlds(cfg, alice, false)
	if err != nil {
		t.Fatalf("org-open listing: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("org-open install worlds = %+v, want both", open)
	}

	// An ambiguous mapping denies closed with a surfaced error.
	cfg.Worlds[1].Allow.Emails = []string{"alice@example.com"}
	got, err := installableWorlds(cfg, alice, true)
	if len(got) != 0 {
		t.Errorf("ambiguous tenant mapping listed worlds: %+v", got)
	}
	var ambiguous errAmbiguousTenant
	if !errors.As(err, &ambiguous) {
		t.Errorf("ambiguous mapping err = %v, want errAmbiguousTenant for the caller's log", err)
	}
}
