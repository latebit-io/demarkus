package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
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
func newMemoryGateway(t *testing.T, cfg *Config, d worldDispatcher) *mcpGateway {
	t.Helper()
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	srv.clock = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return newMCPGateway(srv, "test", d, MemoryGatewayProfile())
}

// seededDispatcher returns a fakeDispatcher whose worlds already carry
// /index.md, so tenantGate's soul seeding takes the already-seeded fast
// path and tests do not see seed publishes unless they want them.
func seededDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		published: map[string]fetch.Result{
			"alice-w/index.md": {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Soul"}},
			"bob-w/index.md":   {Response: protocol.Response{Status: protocol.StatusOK, Body: "# Soul"}},
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

	// Two worlds admitting one identity is a provisioning error and
	// must deny closed, not pick one.
	cfg.Worlds[1].Allow.Emails = []string{"alice@example.com"}
	if _, err := tenantWorldFor(cfg, alice); err == nil || errors.Is(err, ErrNotAuthorized) {
		t.Errorf("tenantWorldFor(ambiguous) err = %v, want a distinct ambiguity error", err)
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

// TestHandleMarkWorldsSelfListsOnlyOwnWorld: the discovery tool must
// never reveal other tenants' world names.
func TestHandleMarkWorldsSelfListsOnlyOwnWorld(t *testing.T) {
	g := newMemoryGateway(t, memoryTestConfig(), seededDispatcher())
	res, err := g.handleMarkWorldsSelf(withAliceClaims(context.Background()), callToolReq("mark_worlds", nil))
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

// TestMemoryGatewayEndToEnd drives the full Streamable HTTP transport:
// initialize carries instructions plus the server name, tools/list is
// exactly the memory surface, and a cross-tenant call is denied at the wire.
func TestMemoryGatewayEndToEnd(t *testing.T) {
	cfg := memoryTestConfig()
	signer := newTestSigner(t)
	verifier := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true}}
	k8s := fake.NewSimpleClientset()
	srv := NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	ts := httptest.NewServer(srv.MemoryMCPGatewayWith("test", seededDispatcher()))
	t.Cleanup(ts.Close)

	resp := mcpRequest(t, ts.URL, "alice-token", "", initializeRequest(1))
	if resp.HTTPStatus != http.StatusOK || resp.Error != nil {
		t.Fatalf("initialize: status=%d err=%+v body=%s", resp.HTTPStatus, resp.Error, resp.RawBody)
	}
	if info, ok := resp.Result["serverInfo"].(map[string]any); !ok || info["name"] != "demarkus-memory-broker" {
		t.Errorf("serverInfo = %+v, want name demarkus-memory-broker", resp.Result["serverInfo"])
	}
	instructions, _ := resp.Result["instructions"].(string)
	if !strings.Contains(instructions, "soul") {
		t.Errorf("initialize instructions missing soul guidance: %q", instructions)
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
