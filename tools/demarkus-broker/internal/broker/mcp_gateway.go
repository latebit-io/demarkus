package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcpGateway is the broker's MCP-over-HTTPS gateway. It owns the mcp-go
// MCPServer and its Streamable HTTP transport, sharing auth + Issuer +
// rate-limit machinery with the parent broker Server.
//
// Slice 1 shipped the listener, the initialize handshake, and the
// tools/list response with all 13 tool definitions. Slice 2 lands
// the real handlers for mark_fetch / mark_list / mark_versions plus
// the per-email sessionCache and per-world worldDispatcher that back
// them. The remaining 10 tools still hit notImplementedHandler.
type mcpGateway struct {
	srv          *Server
	mcpServer    *mcpserver.MCPServer
	transport    *mcpserver.StreamableHTTPServer
	log          *slog.Logger
	sessionCache *sessionCache
	dispatcher   worldDispatcher
}

// newMCPGateway constructs a gateway, registers the 13 tool
// definitions (real handlers for Slice 2's read verbs, placeholders
// for the rest), and wraps the mcp-go MCPServer in a Streamable HTTP
// transport. The transport is an http.Handler — the caller mounts
// it on whichever path/middleware chain it wants (Routes mounts it
// at /mcp behind the gateway auth chain).
//
// version is plumbed through to the MCPServer constructor so the
// initialize response surfaces the broker's build version to MCP
// clients. mcp-go advertises tool capability automatically when AddTool
// is called; resources and prompts stay absent because the plan's
// Out-of-Scope excludes them.
//
// dispatcher is the worldDispatcher backing Slice 2's read handlers.
// Production wires *worldPool; tests inject a fake. Slice 2 also
// constructs the per-process sessionCache here so every tool call
// sees one consistent per-email map.
func newMCPGateway(s *Server, version string, dispatcher worldDispatcher) *mcpGateway {
	mcpSrv := mcpserver.NewMCPServer(
		"demarkus-broker",
		version,
		// listChanged=false: the tool set is static across a session
		// (no hot-add at runtime). Suppresses the
		// notifications/tools/list_changed advertisement the client
		// would otherwise expect us to wire up — we never send one.
		mcpserver.WithToolCapabilities(false),
	)
	g := &mcpGateway{
		srv:          s,
		mcpServer:    mcpSrv,
		log:          s.log,
		sessionCache: newSessionCache(s.cfg.Server.MCP.MaxSessions, s.cfg.Server.MCP.SessionMaxIdle, s.clock),
		dispatcher:   dispatcher,
	}
	g.registerTools()
	g.transport = mcpserver.NewStreamableHTTPServer(mcpSrv)
	return g
}

// registerTools wires the 13 tool definitions onto the MCP server.
// Slice 2 dispatches mark_fetch / mark_list / mark_versions to real
// handlers; the remaining 10 tools fall through to
// notImplementedHandler until later slices replace them. The
// per-tool handler map lives in toolHandlers so adding new
// implementations is a one-line edit and the placeholder fallback
// is impossible to forget.
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

// toolHandlers returns the per-tool handler map. Tools absent
// from the map use notImplementedHandler. Method (rather than a
// package-level var) so the handlers carry the gateway receiver
// without indirection through a global.
func (g *mcpGateway) toolHandlers() map[string]mcpserver.ToolHandlerFunc {
	return map[string]mcpserver.ToolHandlerFunc{
		"mark_fetch":    g.handleMarkFetch,
		"mark_list":     g.handleMarkList,
		"mark_versions": g.handleMarkVersions,
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

// Routes returns the http.Handler for the MCP listener. /mcp is the
// JSON-RPC + SSE endpoint behind the gateway auth chain; the OAuth
// metadata endpoints are unauthenticated by design (RFC 9728 + 8414
// require public discoverability — the whole point of the metadata
// surface is that an unauthenticated client can read it to learn how
// to authenticate).
//
// /.well-known/oauth-authorization-server is registered only when
// Discovery is wired. The handler reuses the existing OIDC discovery
// body — RFC 8414 §3 tolerates extra fields, and the broker is its
// own authorization server so the two metadata documents describe
// the same surface.
func (g *mcpGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	// /mcp accepts both POST (request/notification) and GET (SSE
	// listen channel) per the Streamable HTTP spec. No method filter
	// on the route so mcp-go's transport sees every request type.
	mux.Handle("/mcp", g.srv.gatewayAuth(g.srv.subjectRateLimit(g.transport)))
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", g.oauthProtectedResource)
	if g.srv.discovery != nil {
		mux.Handle("GET /.well-known/oauth-authorization-server", g.srv.discovery.Handler())
	}
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
	pool := newWorldPool(s.cfg, fetch.Options{})
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
