package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// handleMarkWorlds implements the mark_worlds tool: the worlds of this
// knowledge system the calling identity may read, each flagged with whether
// the identity may also write it. Reads are gated by the broker's SSO org gate
// alone (not the per-world Allow, which is the writer allowlist), so this lists
// readableWorlds — every configured world — for any authenticated identity. It
// deliberately does NOT filter the LIST by authorizedWorlds: that hid readable
// worlds from non-writers (the reading-room floor rendered empty for them).
// Instead the writer Allow surfaces per row as the `writable` column, so a
// caller learns both its {readable, writable} sets in one call — the
// access-discovery seam the curation pipeline routes promotions on (readable is
// not writable, the #189 distinction). A pure read over config — no dispatch,
// no Secret access, no per-world traffic.
//
// Output follows the formatToolResult shape (status line, metadata,
// blank line, body) with a markdown table body mirroring mark_lookup's
// table convention, so agents and the library parse one familiar form:
//
//	status: ok
//	count: 2
//
//	| world | url | address | writable |
//	|-------|-----|---------|----------|
//	| team-a | mark://team-a.example.org:6309 | mark://team-a.team-a.svc.cluster.local:6309 | yes |
//	| hub | | mark://hub.hub.svc.cluster.local:6309 | no |
//
// Two addresses, deliberately distinct:
//   - url: the world's externally reachable address (WorldConfig.PublicURL),
//     for a client that wants to connect directly. May be empty.
//   - address: the world's internal dial address (resolveWorldAddress — the
//     same host:port the broker routes to and the federation agent crawls
//     by). Always populated. This is the world's identity in the topology
//     graph (graph.md keys nodes by it), so a consumer like the reading-room
//     floor can join host-keyed graph edges back to the worldName. Internal
//     cluster DNS, but only ever shown to an already-authorized identity.
//
// The worldName remains the addressing primitive for every tool regardless.
func (g *mcpGateway) handleMarkWorlds(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	worlds := readableWorlds(g.srv.cfg)

	var b strings.Builder
	b.WriteString("status: ok\n")
	fmt.Fprintf(&b, "count: %d\n", len(worlds))
	if len(worlds) > 0 {
		b.WriteString("\n| world | url | address | writable |\n|-------|-----|---------|----------|\n")
		for _, w := range worlds {
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
