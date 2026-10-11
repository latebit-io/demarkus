package gateway

import (
	"context"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/mark3labs/mcp-go/mcp"
)

// mcp_tools_federation.go ships the federation READ tool `mark_discover`,
// dispatching unauthenticated like every read.

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
