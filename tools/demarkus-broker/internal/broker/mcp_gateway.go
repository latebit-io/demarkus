package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcpGateway is the broker's MCP-over-HTTPS gateway. It owns the mcp-go
// MCPServer and its Streamable HTTP transport, sharing auth + Issuer +
// rate-limit machinery with the parent broker Server.
//
// Slice 1 (this code) surfaces the listener, the initialize handshake,
// and the tools/list response with all 13 tool definitions. Tool
// implementations stay placeholders that return a structured "not
// implemented" error; Slice 2-5 land the real handlers in dedicated
// files (mcp_tools_read.go, mcp_tools_write.go, mcp_tools_federation*).
type mcpGateway struct {
	srv       *Server
	mcpServer *mcpserver.MCPServer
	transport *mcpserver.StreamableHTTPServer
	log       *slog.Logger
}

// newMCPGateway constructs a gateway, registers the 13 tool
// definitions with the Slice 1 placeholder handler, and wraps the
// mcp-go MCPServer in a Streamable HTTP transport. The transport is
// an http.Handler — the caller mounts it on whichever path/middleware
// chain it wants (Routes mounts it at /mcp behind the gateway auth
// chain).
//
// version is plumbed through to the MCPServer constructor so the
// initialize response surfaces the broker's build version to MCP
// clients. mcp-go advertises tool capability automatically when AddTool
// is called; resources and prompts stay absent because the plan's
// Out-of-Scope excludes them.
func newMCPGateway(s *Server, version string) *mcpGateway {
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
		srv:       s,
		mcpServer: mcpSrv,
		log:       s.log,
	}
	g.registerTools()
	g.transport = mcpserver.NewStreamableHTTPServer(mcpSrv)
	return g
}

// registerTools wires the 13 tool definitions onto the MCP server with
// the Slice 1 placeholder handler. tools/list responses surface every
// tool, but tools/call for any of them returns the structured "not
// implemented" envelope until later slices replace the handler.
func (g *mcpGateway) registerTools() {
	tools := mcpTools()
	for i := range tools {
		g.mcpServer.AddTool(tools[i], g.notImplementedHandler)
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

// MCPGateway returns the MCP gateway's http.Handler. main.go starts a
// second http.Server bound to cfg.Server.MCP.Addr with this handler at
// startup — the gateway is always on; every broker is a knowledge-
// system gateway. version is the broker binary's build version,
// threaded through to the MCP initialize response so clients can
// correlate broker behavior with a specific release.
func (s *Server) MCPGateway(version string) http.Handler {
	return newMCPGateway(s, version).Routes()
}
