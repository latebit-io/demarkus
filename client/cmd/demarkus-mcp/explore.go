package main

import (
	"context"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
)

func (h *handler) markExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.ExploreArgs{URL: rawURL, Render: mcpfmt.Fetch.Options(&req)}
	if mcpfmt.NeighborhoodRequested(&req) {
		args.Relations = &marktools.RelationsArgs{
			Options:    mcpfmt.NeighborhoodOptions(&req),
			Revalidate: mcpfmt.RevalidatesBacklinks(&req),
		}
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Explore(ctx, args) })
}
