package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// handleMarkWorlds lists every world the identity may read (the SSO org gate
// admits all of them) with a writable column from the writer Allow, so one
// call yields both sets (#189). Pure config read; mcpfmt.Full text shape.
func (g *mcpGateway) handleMarkWorlds(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	// Tenant mode scopes the listing to the caller's own world; the
	// writable column then renders yes via the same Allow predicate.
	worlds := g.scopedWorlds(ctx)

	var b strings.Builder
	b.WriteString("status: ok\n")
	fmt.Fprintf(&b, "count: %d\n", len(worlds))
	if len(worlds) > 0 {
		// url is the public address (may be empty); address is the internal
		// dial address, the world's identity in the topology graph.
		b.WriteString("\n| world | url | address | writable |\n|-------|-----|---------|----------|\n")
		for j := range worlds {
			w := &worlds[j]
			fmt.Fprintf(&b, "| %s | %s | mark://%s | %s |\n",
				w.Name, w.PublicURL, resolveWorldAddress(w), yesNo(worldAllows(&w.Allow, claims)))
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

// yesNo renders a writable predicate as a stable table cell. The column is
// the writer-discovery seam the curation pipeline routes on: every world is
// readable (the SSO org gate), but only worlds whose Allow admits the caller
// are writable, so a promotion targets one of these. readable-not-writable is
// exactly the #189 distinction made legible in one call.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// compile-time check that the handler matches mcp-go's expected shape.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkWorlds
