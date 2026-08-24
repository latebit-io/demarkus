package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcpGateway serves 17 MCP-over-HTTPS tools. Reads use broker SSO;
// writes also provision per-world tokens through worldWriteTokens.
type mcpGateway struct {
	srv        *Server
	mcpServer  *mcpserver.MCPServer
	transport  *mcpserver.StreamableHTTPServer
	log        *slog.Logger
	dispatcher worldDispatcher
	// graphStore is an ephemeral in-process graph store backing
	// mark_backlinks / mark_graph / mark_graph_export /
	// mark_graph_publish. Lifetime = broker pod lifetime: any
	// restart drops the store and the agent re-crawls. Plan v5
	// 4b/5 decision (Fritz, 2026-05-21): the broker is a
	// wire-shape adapter, not a stateful protocol surface; an
	// alternate bucket-backed persistent store is parked for a
	// post-broker design window (see /thoughts.md "On Bucket
	// Stores").
	graphStore *graphstore.Store
	// fetchSeen is the per-MCP-session unchanged-fetch dedup state
	// (mcp_tools_fetchmode.go). Session entries are evicted by the
	// OnUnregisterSession hook wired in newMCPGateway; like graphStore
	// it is ephemeral per-pod state, consistent with the wire-shape-
	// adapter framing.
	fetchSeen *sessionSeen
	// graphSeedMu guards graphSeedChecked: worldName → last /graph.md
	// seed check, the per-pod throttle for seedWorldGraph. The seed
	// etag itself lives in graphStore (SeedEtag), keyed by worldName.
	graphSeedMu      sync.Mutex
	graphSeedChecked map[string]time.Time
}

// newMCPGateway registers the 17 tools and wraps them in Streamable HTTP.
// Production supplies *worldPool; tests inject a dispatcher fake.
func newMCPGateway(s *Server, version string, dispatcher worldDispatcher) *mcpGateway {
	// Session-end eviction for the fetch dedup state. The store is
	// constructed before the MCPServer because the hook closes over it.
	fetchSeen := newSessionSeen(s.log, s.clock)
	hooks := &mcpserver.Hooks{}
	hooks.AddOnUnregisterSession(func(_ context.Context, session mcpserver.ClientSession) {
		if session != nil {
			fetchSeen.drop(session.SessionID())
		}
	})
	mcpSrv := mcpserver.NewMCPServer(
		"demarkus-broker",
		version,
		// listChanged=false: the tool set is static across a session
		// (no hot-add at runtime). Suppresses the
		// notifications/tools/list_changed advertisement the client
		// would otherwise expect us to wire up — we never send one.
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithHooks(hooks),
	)
	g := &mcpGateway{
		srv:        s,
		mcpServer:  mcpSrv,
		log:        s.log,
		dispatcher: dispatcher,
		// Ephemeral graph store — empty on each gateway
		// construction, dies with the broker pod. See the
		// graphStore field doc for the design framing.
		graphStore: graphstore.New(),
		fetchSeen:  fetchSeen,
	}
	g.registerTools()
	g.registerResources()
	g.registerPrompts()
	g.transport = mcpserver.NewStreamableHTTPServer(mcpSrv)
	return g
}

// registerTools wires all definitions to their handlers. Missing handlers use
// notImplementedHandler so an incomplete registration fails visibly.
func (g *mcpGateway) registerTools() {
	handlers := g.toolHandlers()
	tools := mcpTools()
	for i := range tools {
		h, ok := handlers[tools[i].Name]
		if !ok {
			h = g.notImplementedHandler
		}
		g.mcpServer.AddTool(tools[i], h)
	}
}

// toolHandlers maps all 17 tools to concrete implementations. Keeping this a
// method binds handlers to the gateway without global indirection.
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
	mux.Handle(mcpPath, g.srv.gatewayAuth(g.srv.subjectRateLimit(g.transport)))
	mux.HandleFunc("GET "+prmPath, g.oauthProtectedResource)
	// prmPath+mcpPath is RFC 9728 §3.1's path-inserted form for a
	// resource URL that carries a path. Same document.
	mux.HandleFunc("GET "+prmPath+mcpPath, g.oauthProtectedResource)
	return mux
}

// MCPGateway returns the MCP gateway's http.Handler with the
// production worldPool wired as the dispatcher. main.go starts a
// second http.Server bound to cfg.Server.MCP.Addr with this handler
// at startup — the gateway is always on; every broker is a
// knowledge-system gateway. version is the broker binary's build
// version, threaded through to the MCP initialize response so
// clients can correlate broker behavior with a specific release.
//
// The returned worldPool's lifecycle is bound to the gateway;
// main.go calls (*Server).CloseMCPGateway during shutdown to drain
// pooled QUIC connections. For test injection use MCPGatewayWith.
func (s *Server) MCPGateway(version string) http.Handler {
	// WorldDialer.InsecureSkipVerify propagates into fetch.Options.Insecure
	// so the QUIC client skips TLS chain + hostname verification on
	// dial. Required today for in-cluster deployments using per-world
	// self-signed certs (no shared CA the broker can trust); the flag
	// stays opt-in so the secure-by-default posture is preserved.
	// See WorldDialerConfig doc for the trust-model rationale.
	pool := newWorldPool(s.cfg, fetch.Options{
		Insecure: s.cfg.WorldDialer.InsecureSkipVerify,
	})
	s.mcpPool = pool
	return newMCPGateway(s, version, pool).Routes()
}

// MCPGatewayWith returns the MCP gateway's http.Handler using the
// supplied dispatcher. Test seam for injecting a fakeDispatcher
// without forcing every test to spin up a real QUIC listener. The
// production path always goes through MCPGateway, which owns the
// worldPool lifecycle.
func (s *Server) MCPGatewayWith(version string, dispatcher worldDispatcher) http.Handler {
	return newMCPGateway(s, version, dispatcher).Routes()
}

// CloseMCPGateway closes the production worldPool wired by
// MCPGateway, releasing pooled QUIC connections. No-op when called
// against a Server that never built a production gateway (tests use
// MCPGatewayWith and own their fake dispatcher's lifecycle directly).
func (s *Server) CloseMCPGateway() {
	if s.mcpPool != nil {
		s.mcpPool.Close()
		s.mcpPool = nil
	}
}
