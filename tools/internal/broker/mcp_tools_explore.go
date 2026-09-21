package broker

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// handleMarkExplore answers mark_explore: a card that orients an agent on one
// document without its body. Reads go out unauthenticated, like every read.
func (g *mcpGateway) handleMarkExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.ExploreArgs{URL: raw, Render: mcpfmt.Fetch.Options(&req)}
	if mcpfmt.NeighborhoodRequested(&req) {
		args.Relations = &marktools.RelationsArgs{
			Options:    mcpfmt.NeighborhoodOptions(&req),
			Revalidate: mcpfmt.RevalidatesBacklinks(&req),
		}
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Explore(ctx, args) })
}
