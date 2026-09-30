package gateway

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
)

// handleMarkExplore answers mark_explore: a card that orients an agent on one
// document without its body. Reads go out unauthenticated, like every read.
func (g *Gateway) handleMarkExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	args, err := mcpbind.Explore(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Explore(ctx, args) })
}
