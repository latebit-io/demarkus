package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Deps is what one MCP gateway needs from the rest of the broker.
// Run fills it per profile; tests fill it from a fixture.
type Deps struct {
	// Worlds is the registry as this gateway's profile sees it.
	Worlds *core.WorldView
	// Issuer is the configured IdP issuer, the first half of every identity key.
	Issuer string
	// PublicURL is the authorization server the gateway names to clients.
	PublicURL string
	// Gateway carries the gateway's own URL and tool profile.
	Gateway core.GatewayConfig
	// Realm names the product in the 401 challenge; blank takes the profile's.
	Realm string
	// SharedDeps is the verifier, subject limiter, clock and log the
	// management API uses too; AllowDomains is the broker wide domain gate.
	core.SharedDeps
	AllowDomains []string
	// Provisioner creates tenant worlds on first arrival; nil in static mode.
	Provisioner *storage.Provisioner
	// Hub is the world holding the federation checkpoints of the Federated
	// worlds, whose graphs seed from them; the rest seed from their own.
	Hub       string
	Federated map[string]bool
}

// DepsFor builds one profile's gateway deps from what the management API
// shares. Run and the fixtures both build deps here.
func DepsFor(cfg *core.Config, profile *Profile, shared core.SharedDeps, provisioner *storage.Provisioner) *Deps {
	var federated map[string]bool
	if cfg.Federation.Hub != "" && !profile.TenantScoped { // a tenant's graph never leaves its own world
		federated = map[string]bool{}
		for _, name := range cfg.FederatedWorlds() {
			federated[name] = true
		}
	}
	return &Deps{
		Hub:          cfg.Federation.Hub,
		Federated:    federated,
		Worlds:       cfg.Registry().View(profile.Name),
		Issuer:       cfg.OIDC.Issuer,
		PublicURL:    cfg.Server.PublicURL,
		Gateway:      cfg.Server.Gateway(profile.Name),
		Realm:        cfg.Server.Realm,
		SharedDeps:   shared,
		AllowDomains: cfg.OIDC.AllowDomains,
		Provisioner:  provisioner,
	}
}

// Gateway serves 17 MCP-over-HTTPS tools behind broker SSO. Writes run
// under the identity's grant on the world; no world token is involved.
type Gateway struct {
	deps       *Deps
	mcpServer  *mcpserver.MCPServer
	transport  *mcpserver.StreamableHTTPServer
	dispatcher WorldDispatcher
	// resource is this gateway's RFC 8707 indicator, the PRM `resource`.
	resource string
	// profile selects the product surface (knowledge vs memory):
	// tool set, instructions, tenant scoping, memory seeding.
	profile *Profile
	// Knowledge graphs are shared; memory calls select an isolated tenant graph.
	knowledgeGraph *gatewayGraph
	tenantGraphsMu sync.Mutex
	tenantGraphs   map[string]*tenantGraph
	// Session-end hooks evict unchanged-fetch dedup state.
	fetchSeen *sessionSeen
	// memorySeed tracks per-world memory-template seeding (memory profile).
	memorySeed memorySeeder
	// tools are the shared mark_* bodies bound to this gateway; nil only if
	// the gateway was built without a dispatcher, which run reports.
	tools *marktools.Tools
}

// gatewayGraph keeps graph data and refresh state in the same isolation scope.
// Both are ephemeral and disappear on broker restart.
type gatewayGraph struct {
	graphStore *graphstore.Store
	tenant     string // empty for the organizational graph
}

type tenantGraph struct {
	identity string
	address  string
	dial     string
	graph    *gatewayGraph
	lastUsed time.Time
}

// Idle tenant graphs retire so a long-lived pod does not hold every tenant's
// seed forever; the cap bounds memory under churn. Retired scopes re-seed.
const (
	tenantGraphTTL  = time.Hour
	maxTenantGraphs = 256
)

// graphFor resolves scope on every call. Identity or backend changes retire
// the entire graph, including etags; in-flight work retains only the old graph.
func (g *Gateway) graphFor(ctx context.Context) (*gatewayGraph, error) {
	if !g.profile.TenantScoped {
		return g.knowledgeGraph, nil
	}
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return nil, err
	}
	claims, ok := core.ClaimsFromCtx(ctx)
	if !ok {
		return nil, core.ErrNotAuthorized
	}
	identity := core.IdentityKey(g.deps.Issuer, claims.Subject)
	address := w.Address()
	now := g.deps.Clock()
	g.tenantGraphsMu.Lock()
	defer g.tenantGraphsMu.Unlock()
	g.retireIdleTenantGraphsLocked(now, w.Name)
	entry := g.tenantGraphs[w.Name]
	if entry == nil || entry.identity != identity || entry.address != address || entry.dial != w.DialAddress {
		if entry == nil {
			g.retireOldestTenantGraphsLocked()
		}
		entry = &tenantGraph{
			identity: identity, address: address, dial: w.DialAddress,
			graph: &gatewayGraph{graphStore: graphstore.New(), tenant: w.Name},
		}
		g.tenantGraphs[w.Name] = entry
	}
	entry.lastUsed = now
	return entry.graph, nil
}

func (g *Gateway) retireIdleTenantGraphsLocked(now time.Time, keep string) {
	for world, entry := range g.tenantGraphs {
		if world != keep && now.Sub(entry.lastUsed) > tenantGraphTTL {
			delete(g.tenantGraphs, world)
		}
	}
}

// retireOldestTenantGraphsLocked makes room for one more scope under the cap.
func (g *Gateway) retireOldestTenantGraphsLocked() {
	for len(g.tenantGraphs) >= maxTenantGraphs {
		var oldest string
		var oldestAt time.Time
		for world, entry := range g.tenantGraphs {
			if oldest == "" || entry.lastUsed.Before(oldestAt) {
				oldest, oldestAt = world, entry.lastUsed
			}
		}
		delete(g.tenantGraphs, oldest)
	}
}

// Failed resolution has no world name. Retire every scope owned by the identity
// so later reauthorization cannot reuse its graph or seed bookkeeping.
func (g *Gateway) evictTenantGraphs(identity string) {
	g.tenantGraphsMu.Lock()
	defer g.tenantGraphsMu.Unlock()
	for world, entry := range g.tenantGraphs {
		if entry.identity == identity {
			delete(g.tenantGraphs, world)
		}
	}
}

// dropWorld forgets what this gateway cached for a world that left the
// registry, so a world provisioned later under its name starts clean.
func (g *Gateway) dropWorld(name string) {
	g.memorySeed.forget(name)
	g.tenantGraphsMu.Lock()
	delete(g.tenantGraphs, name)
	g.tenantGraphsMu.Unlock()
}

// New narrows the profile's tools to the configured tool profile, registers
// them and wraps them in Streamable HTTP. Production supplies *WorldPool;
// tests inject a dispatcher fake.
func New(deps *Deps, version string, dispatcher WorldDispatcher, profile *Profile) *Gateway {
	narrowed := *profile
	narrowed.Tools = mcpfmt.ProfileTools(deps.Gateway.ToolProfile, profile.Tools)
	profile = &narrowed
	// Session-end eviction for the fetch dedup state. The store is
	// constructed before the MCPServer because the hook closes over it.
	fetchSeen := newSessionSeen(deps.Log, deps.Clock)
	hooks := &mcpserver.Hooks{}
	hooks.AddOnUnregisterSession(func(_ context.Context, session mcpserver.ClientSession) {
		if session != nil {
			fetchSeen.drop(session.SessionID())
		}
	})
	g := &Gateway{
		deps:           deps,
		dispatcher:     dispatcher,
		resource:       deps.Gateway.Resource(),
		profile:        profile,
		knowledgeGraph: &gatewayGraph{graphStore: graphstore.New()},
		tenantGraphs:   make(map[string]*tenantGraph),
		fetchSeen:      fetchSeen,
	}
	deps.Worlds.OnDrop(g.dropWorld)
	tools, err := g.toolBodies()
	if err != nil {
		deps.Log.Error("mcp gateway: tool bodies unavailable", "err", err)
	}
	g.tools = tools
	opts := []mcpserver.ServerOption{
		// listChanged=false: the tool set is static across a session,
		// so we never send notifications/tools/list_changed.
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithHooks(hooks),
	}
	if profile.Instructions != "" {
		opts = append(opts, mcpserver.WithInstructions(profile.Instructions))
	}
	if profile.TenantScoped {
		// Every tool call passes the tenant gate BEFORE its handler:
		// identity resolves to exactly one world and every
		// world-bearing argument must address it.
		opts = append(opts, mcpserver.WithToolHandlerMiddleware(g.tenantGate))
	}
	g.mcpServer = mcpserver.NewMCPServer(profile.ServerName, version, opts...)
	g.registerTools()
	g.registerResources()
	g.registerPrompts()
	g.transport = g.newTransport(sessionIdleTTL)
	return g
}

// sessionIdleTTL frees a session its client left without a DELETE, which
// Claude Code never sends; a later call on the swept ID is still served. No
// heartbeat: each ping's reply would spend the caller's subject rate limit.
const sessionIdleTTL = 30 * time.Minute

// newTransport is the Streamable HTTP transport with the session sweep and
// its log lines on the gateway logger.
func (g *Gateway) newTransport(idleTTL time.Duration) *mcpserver.StreamableHTTPServer {
	return mcpserver.NewStreamableHTTPServer(g.mcpServer,
		mcpserver.WithSessionIdleTTL(idleTTL),
		mcpserver.WithStreamableHTTPLogger(g.deps.Log))
}

// Shutdown stops the session sweep and closes the open sessions; the HTTP
// listener that mounted the gateway is the caller's.
func (g *Gateway) Shutdown(ctx context.Context) error {
	return g.transport.Shutdown(ctx)
}

// registerTools wires the profile's definitions to their handlers. Missing
// handlers use notImplementedHandler so an incomplete registration fails
// visibly.
func (g *Gateway) registerTools() {
	handlers := g.toolHandlers()
	tools := g.profile.Tools
	for i := range tools {
		h, ok := handlers[tools[i].Name]
		if !ok {
			h = g.notImplementedHandler
		}
		g.mcpServer.AddTool(tools[i], h)
	}
}

// toolHandlers maps tools to implementations; registerTools wires only
// the profile's tool list.
func (g *Gateway) toolHandlers() map[string]mcpserver.ToolHandlerFunc {
	return map[string]mcpserver.ToolHandlerFunc{
		"mark_fetch":         g.handleMarkFetch,
		"mark_explore":       g.handleMarkExplore,
		"mark_list":          g.handleMarkList,
		"mark_lookup":        g.handleMarkLookup,
		"mark_lookup_all":    g.handleMarkLookupAll,
		"mark_versions":      g.handleMarkVersions,
		"mark_publish":       g.handleMarkPublish,
		"mark_append":        g.handleMarkAppend,
		"mark_archive":       g.handleMarkArchive,
		"mark_discover":      g.handleMarkDiscover,
		"mark_resolve":       g.handleMarkResolve,
		"mark_backlinks":     g.handleMarkBacklinks,
		"mark_graph":         g.handleMarkGraph,
		"mark_index":         g.handleMarkIndex,
		"mark_graph_export":  g.handleMarkGraphExport,
		"mark_graph_publish": g.handleMarkGraphPublish,
		"mark_worlds":        g.handleMarkWorlds,
	}
}

// notImplementedHandler is the Slice 1 placeholder. Returns an MCP
// tool-error envelope so the client sees a structured failure rather
// than a generic 500 — the same shape later slices use when real
// handlers fail. The tool name in the error message makes it obvious
// at log-trace time which call hit the placeholder.
func (g *Gateway) notImplementedHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	return mcp.NewToolResultError(fmt.Sprintf("tool %q not yet implemented (Slice 1 gateway foundation)", req.Params.Name)), nil
}

// prmPath is the RFC 9728 metadata route; core.MCPPath is the endpoint.
const prmPath = "/.well-known/oauth-protected-resource"

// Register mounts the gateway on a shared mux: a host binds the routes to
// that hostname, blank answers every host the mux does not route elsewhere.
// Metadata is unauthenticated (RFC 9728); AS metadata stays on the issuer (RFC 8414 §3.3).
func (g *Gateway) Register(mux *http.ServeMux, host string) {
	// /mcp accepts both POST (request/notification) and GET (SSE
	// listen channel) per the Streamable HTTP spec. No method filter
	// on the route so mcp-go's transport sees every request type.
	mux.Handle(host+core.MCPPath, g.gatewayAuth(core.SubjectRateLimit(g.deps.SubjectLimiter, g.deps.Log, g.transport)))
	mux.HandleFunc("GET "+host+prmPath, g.oauthProtectedResource)
	// prmPath+MCPPath is RFC 9728 §3.1's path-inserted form for a
	// resource URL that carries a path. Same document.
	mux.HandleFunc("GET "+host+prmPath+core.MCPPath, g.oauthProtectedResource)
}

// Routes is the gateway alone on a mux, for tests and single-gateway hosts.
func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	g.Register(mux, "")
	return mux
}
