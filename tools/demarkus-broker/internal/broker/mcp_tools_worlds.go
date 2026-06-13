package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// handleMarkWorlds implements the mark_worlds tool: the worlds of this
// knowledge system the calling identity may read, per the same
// authorizedWorlds predicate /auth/callback and /me/install already
// apply. A pure read over config and claims — no dispatch, no Secret
// access, no per-world traffic.
//
// Output follows the formatToolResult shape (status line, metadata,
// blank line, body) with a markdown table body mirroring mark_lookup's
// table convention, so agents and the library parse one familiar form:
//
//	status: ok
//	count: 2
//
//	| world | url |
//	|-------|-----|
//	| team-a | mark://team-a.example.org:6309 |
//	| hub | |
//
// url is the world's externally reachable address (WorldConfig.PublicURL)
// and may be empty — the worldName remains the addressing primitive for
// every tool either way.
func (g *mcpGateway) handleMarkWorlds(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	worlds := authorizedWorlds(g.srv.cfg, claims)

	var b strings.Builder
	b.WriteString("status: ok\n")
	fmt.Fprintf(&b, "count: %d\n", len(worlds))
	if len(worlds) > 0 {
		b.WriteString("\n| world | url |\n|-------|-----|\n")
		for _, w := range worlds {
			fmt.Fprintf(&b, "| %s | %s |\n", w.Name, w.PublicURL)
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

// compile-time check that the handler matches mcp-go's expected shape.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkWorlds
