package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// gatewayDeps is what the MCP gateway needs from the rest of the broker.
// Run fills it once for both listeners; tests fill it from a fixture.
type gatewayDeps struct {
	Worlds *worldRegistry
	// Issuer is the configured IdP issuer, the first half of every identity key.
	Issuer string
	// PublicURL is the authorization server the gateway names to clients.
	PublicURL string
	// MCP carries the gateway's own URL and the first mint retry knobs.
	MCP MCPConfig
	// Realm names the product in the 401 challenge.
	Realm string
	// Verifier is the same composed verifier the management API uses;
	// AllowDomains is the broker wide hosted domain gate.
	Verifier     Verifier
	AllowDomains []string
	// SubjectLimiter is shared with /me/install; nil passes through.
	SubjectLimiter *rateLimitRegistry
	Clock          func() time.Time
	Log            *slog.Logger
	WriteTokens    *worldWriteTokenStore
	// Provisioner creates tenant worlds on first arrival; nil in static mode.
	Provisioner *Provisioner
}

// mcpGateway serves 17 MCP-over-HTTPS tools. Reads use broker SSO;
// writes also provision per-world tokens through deps.WriteTokens.
type mcpGateway struct {
	deps       *gatewayDeps
	mcpServer  *mcpserver.MCPServer
	transport  *mcpserver.StreamableHTTPServer
	dispatcher worldDispatcher
	// profile selects the product surface (knowledge vs memory):
	// tool set, instructions, tenant scoping, memory seeding.
	profile *GatewayProfile
	// Knowledge graphs are shared; memory calls select an isolated tenant graph.
	knowledgeGraph *gatewayGraph
	tenantGraphsMu sync.Mutex
	tenantGraphs   map[string]*tenantGraph
	// Session-end hooks evict unchanged-fetch dedup state.
	fetchSeen *sessionSeen
	// memorySeed tracks per-world memory-template seeding (memory profile).
	memorySeed memorySeeder
	// refusals spares a refused identity a registry round trip on every call.
	refusals tenantRefusals
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
func (g *mcpGateway) graphFor(ctx context.Context) (*gatewayGraph, error) {
	if !g.profile.TenantScoped {
		return g.knowledgeGraph, nil
	}
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return nil, err
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return nil, ErrNotAuthorized
	}
	identity := identityKey(g.deps.Issuer, claims.Subject)
	address := resolveWorldAddress(&w)
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

func (g *mcpGateway) retireIdleTenantGraphsLocked(now time.Time, keep string) {
	for world, entry := range g.tenantGraphs {
		if world != keep && now.Sub(entry.lastUsed) > tenantGraphTTL {
			delete(g.tenantGraphs, world)
		}
	}
}

// retireOldestTenantGraphsLocked makes room for one more scope under the cap.
func (g *mcpGateway) retireOldestTenantGraphsLocked() {
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
func (g *mcpGateway) evictTenantGraphs(identity string) {
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
func (g *mcpGateway) dropWorld(name string) {
	g.memorySeed.forget(name)
	g.tenantGraphsMu.Lock()
	delete(g.tenantGraphs, name)
	g.tenantGraphsMu.Unlock()
}

// newMCPGateway registers the profile's tools and wraps them in Streamable
// HTTP. Production supplies *worldPool; tests inject a dispatcher fake.
func newMCPGateway(deps *gatewayDeps, version string, dispatcher worldDispatcher, profile *GatewayProfile) *mcpGateway {
	// Session-end eviction for the fetch dedup state. The store is
	// constructed before the MCPServer because the hook closes over it.
	fetchSeen := newSessionSeen(deps.Log, deps.Clock)
	hooks := &mcpserver.Hooks{}
	hooks.AddOnUnregisterSession(func(_ context.Context, session mcpserver.ClientSession) {
		if session != nil {
			fetchSeen.drop(session.SessionID())
		}
	})
	g := &mcpGateway{
		deps:           deps,
		dispatcher:     dispatcher,
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
	g.transport = mcpserver.NewStreamableHTTPServer(g.mcpServer)
	return g
}

// registerTools wires the profile's definitions to their handlers. Missing
// handlers use notImplementedHandler so an incomplete registration fails
// visibly.
func (g *mcpGateway) registerTools() {
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
func (g *mcpGateway) toolHandlers() map[string]mcpserver.ToolHandlerFunc {
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
func (g *mcpGateway) notImplementedHandler(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	return mcp.NewToolResultError(fmt.Sprintf("tool %q not yet implemented (Slice 1 gateway foundation)", req.Params.Name)), nil
}

// mcpPath is the gateway's JSON-RPC endpoint path. The RFC 9728
// `resource` field and the path-inserted metadata route derive from
// it so the three can never drift apart.
const (
	mcpPath = "/mcp"
	prmPath = "/.well-known/oauth-protected-resource"
)

// Routes returns the http.Handler for the MCP listener. Metadata routes
// are unauthenticated by design (RFC 9728/8414 discovery). AS metadata is
// NOT served here: RFC 8414 §3.3 wants the issuer's origin, not the gateway's.
func (g *mcpGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	// /mcp accepts both POST (request/notification) and GET (SSE
	// listen channel) per the Streamable HTTP spec. No method filter
	// on the route so mcp-go's transport sees every request type.
	mux.Handle(mcpPath, g.gatewayAuth(subjectRateLimit(g.deps.SubjectLimiter, g.deps.Log, g.transport)))
	mux.HandleFunc("GET "+prmPath, g.oauthProtectedResource)
	// prmPath+mcpPath is RFC 9728 §3.1's path-inserted form for a
	// resource URL that carries a path. Same document.
	mux.HandleFunc("GET "+prmPath+mcpPath, g.oauthProtectedResource)
	return mux
}
