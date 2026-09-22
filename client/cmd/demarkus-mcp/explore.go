package main

import (
	"context"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/mark3labs/mcp-go/mcp"
)

func (h *handler) markExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Explore(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return h.run(func(t *marktools.Tools) marktools.Result { return t.Explore(ctx, args) })
}
