package gateway

import (
	"context"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/mark3labs/mcp-go/mcp"
)

// mcp_tools_federation.go ships the federation READ tools `mark_discover`
// and `mark_resolve`, dispatching unauthenticated like every read.
// Cross-knowledge-system resolution is out of scope: unknown servers skip.

// handleMarkDiscover answers mark_discover: the world's agent manifest.
func (g *Gateway) handleMarkDiscover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	// url is required here, unlike demarkus-mcp: the shared body reads an
	// empty one as "the default server", which the gateway does not have.
	raw, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Discover(ctx, raw) })
}

// handleMarkResolve answers mark_resolve. A candidate server is a world here,
// so one outside this system is skipped, not fetched.
func (g *Gateway) handleMarkResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Resolve(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.ResolveHash(ctx, args) })
}
