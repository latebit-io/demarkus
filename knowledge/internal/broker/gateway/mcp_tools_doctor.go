package gateway

import (
	"context"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/mark3labs/mcp-go/mcp"
)

// mark_doctor is a read-only audit; the tenant gate scopes its url like a read.
func (g *Gateway) handleMarkDoctor(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Doctor(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Doctor(ctx, args) })
}
