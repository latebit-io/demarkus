package broker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcp_tools_write.go ships the three write-verb handlers
// (mark_publish, mark_append, mark_archive) for Slice 3 of the
// broker MCP gateway plan. All three reuse dispatchWithAuth so
// the propagation-race retry + cache-hit-401 invalidation built
// in Slice 2 work for writes too.
//
// Slice 3 deliberately ships on_conflict="fail" only for
// mark_publish; the merge-candidate flow (on_conflict="merge")
// lands in Slice 6 along with the hoisted client/merge package.
// Until then the broker rejects on_conflict="merge" with a
// pointer to Slice 6 so agents calling against the broker know
// the surface gap and can either pass "fail" or wait. The
// default flips to "merge" in Slice 6 to match the local
// demarkus-mcp.

// agentMetaFromClaims builds the publisher-metadata map every
// write request carries. Mirrors the local demarkus-mcp's
// agentMeta(ctx) shape — "agent": "<MCP client name from session>"
// — but the broker has no session-level client info available on
// the tool-call context, so we use the canonical email instead.
// That makes the world's audit log show who initiated the write
// even when the agent never tells the world its name; the
// broker's existing audit log already correlates by email so the
// two surfaces agree.
func agentMetaFromClaims(claims Claims) map[string]string {
	return map[string]string{"agent": canonicalEmail(claims.Email)}
}

// handleMarkPublish implements the mark_publish tool. on_conflict
// defaults to "fail" in Slice 3; the "merge" branch (Slice 6)
// returns a tool error pointing the caller at the planned
// surface so the gap is loud, not silent.
func (g *mcpGateway) handleMarkPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError("body is required"), nil
	}
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	expectedVersion, err := req.RequireInt("expected_version")
	if err != nil {
		return mcp.NewToolResultError("expected_version is required"), nil
	}
	// Validate before the on_conflict switch — both the "fail"
	// branch and the future "merge" branch reject negative
	// versions in the same shape, surfacing a clear local error
	// instead of forwarding it to the world.
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}
	onConflict := strings.TrimSpace(req.GetString("on_conflict", "fail"))
	if onConflict == "" {
		onConflict = "fail"
	}
	switch onConflict {
	case "fail":
		// Pass through to the dispatcher; the world returns
		// `conflict` status when expectedVersion doesn't match,
		// and we forward that verbatim.
	case "merge":
		// Slice 6 brings the structural-merge candidate flow.
		// Until then, fail loud with a pointer to the planned
		// landing point so the agent knows to retry with
		// on_conflict="fail" (or wait for Slice 6).
		return mcp.NewToolResultError("on_conflict=\"merge\" not yet supported by broker MCP gateway (Slice 6 of /plans/broker-https-gateway.md lands the merge-candidate flow); pass on_conflict=\"fail\" for now"), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid on_conflict %q: expected \"fail\" (\"merge\" lands in Slice 6)", onConflict)), nil
	}

	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	meta := agentMetaFromClaims(claims)
	result, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.Publish(worldName, path, body, token, expectedVersion, meta)
	})
	if err != nil {
		return g.toolErrorFor("publish", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "server-version")), nil
}

// handleMarkAppend implements the mark_append tool. expected_version
// is optional: when omitted or 0, the broker auto-resolves by
// calling VERSIONS first and using `current` as the expected
// version for the APPEND. Matches the local demarkus-mcp's
// behavior so agents see the same convenience either way.
//
// The auto-resolve path issues two dispatch calls under one
// session's cached token — the singleflight + sessionCache from
// Slice 2 ensure only the very first call pays the mint round
// trip; the VERSIONS + APPEND pair shares whatever's already
// cached.
func (g *mcpGateway) handleMarkAppend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError("body is required"), nil
	}
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	expectedVersion := req.GetInt("expected_version", 0)
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	if expectedVersion == 0 {
		// Auto-resolve via VERSIONS. Same dispatch wrapper as
		// the actual append so a propagation-race or stale-
		// cache 401 here is absorbed by the retry loop and
		// doesn't fail the whole append call.
		vResult, vErr := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
			return g.dispatcher.Versions(worldName, path, token)
		})
		if vErr != nil {
			return g.toolErrorFor("append (auto-resolve)", worldName, vErr), nil
		}
		if vResult.Response.Status != protocol.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve version: %s", vResult.Response.Status)), nil
		}
		cur, exists := vResult.Response.Metadata["current"]
		if !exists {
			return mcp.NewToolResultError("could not resolve version: no current version in response"), nil
		}
		resolved, parseErr := strconv.Atoi(cur)
		if parseErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not resolve version: invalid current version %q", cur)), nil
		}
		expectedVersion = resolved
	}
	meta := agentMetaFromClaims(claims)
	result, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.Append(worldName, path, body, token, expectedVersion, meta)
	})
	if err != nil {
		return g.toolErrorFor("append", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "server-version")), nil
}

// handleMarkArchive implements the mark_archive tool. ARCHIVE is
// the demarkus protocol's destructive verb: status flips to
// `archived` and the document stops appearing in LIST results,
// but version history is preserved. Output keys mirror the local
// demarkus-mcp choice (`version`).
func (g *mcpGateway) handleMarkArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	result, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.Archive(worldName, path, token)
	})
	if err != nil {
		return g.toolErrorFor("archive", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "version")), nil
}

// Compile-time guard: the write handlers conform to mcp-go's
// ToolHandlerFunc shape. Catches a signature regression at build
// time instead of runtime registration.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkPublish
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkAppend
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkArchive
