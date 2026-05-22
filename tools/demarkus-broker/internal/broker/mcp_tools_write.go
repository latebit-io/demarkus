package broker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/client/merge"
	"github.com/latebit/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcp_tools_write.go ships the three write-verb handlers
// (mark_publish, mark_append, mark_archive). All three reuse
// dispatchWithAuth so the propagation-race retry +
// cache-hit-401 invalidation built in Slice 2 apply to writes
// too.
//
// Slice 3 shipped the write verbs with on_conflict="fail" only;
// Slice 6 lands the merge-candidate flow (on_conflict="merge")
// via the client/merge package and the brokerMergeAdapter
// below. Default on_conflict is now "merge" — matches the local
// demarkus-mcp's surface and prevents the silent-content-loss
// footgun that "fail" leaves open when a naive caller retries
// with a stale body.

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
// defaults to "merge" (matches local demarkus-mcp); pass
// on_conflict="fail" to opt out of the merge candidate flow and
// get the raw conflict response instead.
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
	// Validate before the on_conflict switch — both branches
	// reject negative versions identically. Forwarding a
	// negative value to the world would be a server-side error
	// surfaced as a tool error anyway; doing it locally gives
	// a clearer message and saves a round trip.
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}
	// Default flipped to "merge" in Slice 6 — matches the local
	// demarkus-mcp's surface. An explicit empty or whitespace-
	// only on_conflict normalizes to the default since MCP
	// clients commonly send blank strings for optional fields;
	// rejecting them would turn the default path into a needless
	// tool error.
	onConflict := strings.TrimSpace(req.GetString("on_conflict", "merge"))
	if onConflict == "" {
		onConflict = "merge"
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	meta := agentMetaFromClaims(claims)
	switch onConflict {
	case "fail":
		// Pass through to the dispatcher; the world returns
		// `conflict` status when expectedVersion doesn't match,
		// and we forward that verbatim. Same shape the
		// pre-Slice-6 surface exposed.
		result, perr := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
			return g.dispatcher.Publish(worldName, path, body, token, expectedVersion, meta)
		})
		if perr != nil {
			return g.toolErrorFor("publish", worldName, perr), nil
		}
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "server-version")), nil
	case "merge":
		adapter := &brokerMergeAdapter{
			g:         g,
			ctx:       ctx,
			claims:    claims,
			worldName: worldName,
		}
		outcome, mErr := merge.Candidate(adapter, path, body, expectedVersion, meta)
		if mErr != nil {
			return g.toolErrorFor("publish", worldName, mErr), nil
		}
		return mcp.NewToolResultText(formatMergeOutcome(&outcome)), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid on_conflict %q: expected \"merge\" or \"fail\"", onConflict)), nil
	}
}

// brokerMergeAdapter satisfies merge.Client by routing each
// underlying call through dispatchWithAuth, so the
// FetchVersion / FetchCurrent / Publish sequence that
// merge.Candidate orchestrates inherits the broker's session
// cache + propagation-race retry built in Slice 2.
//
// ctx is captured in the struct because merge.Client's method
// signatures don't carry context — the local demarkus-mcp's
// equivalent adapter has the same shape against fetch.Client.
// The adapter's lifetime is bounded by one handleMarkPublish
// call, so capturing the handler's ctx is the load-bearing
// alternative to leaking ctx.Done propagation across the
// merge orchestration.
type brokerMergeAdapter struct {
	g         *mcpGateway
	ctx       context.Context
	claims    Claims
	worldName string
}

// FetchVersion fetches a specific historical version via the
// /path/vN suffix the server understands. Same convention the
// local demarkus-mcp uses; mirroring it keeps the merge base/
// theirs/ours triple consistent across direct-QUIC and
// brokered access.
func (a *brokerMergeAdapter) FetchVersion(path string, version int) (merge.Doc, error) {
	versionedPath := strings.TrimRight(path, "/") + "/v" + strconv.Itoa(version)
	r, err := a.g.dispatchWithAuth(a.ctx, a.claims, a.worldName, func(token string) (fetch.Result, error) {
		return a.g.dispatcher.Fetch(a.worldName, versionedPath, token)
	})
	if err != nil {
		return merge.Doc{}, err
	}
	return mergeDocFromResult(r)
}

// FetchCurrent fetches the head version of path. Single
// dispatchWithAuth round trip; cache hit if the session already
// fetched this path within idle window.
func (a *brokerMergeAdapter) FetchCurrent(path string) (merge.Doc, error) {
	r, err := a.g.dispatchWithAuth(a.ctx, a.claims, a.worldName, func(token string) (fetch.Result, error) {
		return a.g.dispatcher.Fetch(a.worldName, path, token)
	})
	if err != nil {
		return merge.Doc{}, err
	}
	return mergeDocFromResult(r)
}

// Publish forwards to the dispatcher and lifts the protocol
// response into a merge.PublishResult. Status, version, and
// server-version come from the response metadata; the
// full metadata map rides along for downstream formatters
// (formatMergeOutcome.OutcomeOK delegates to formatToolResult
// which echoes the published version/modified/server-version
// keys).
func (a *brokerMergeAdapter) Publish(path, body string, expectedVersion int, meta map[string]string) (merge.PublishResult, error) {
	r, err := a.g.dispatchWithAuth(a.ctx, a.claims, a.worldName, func(token string) (fetch.Result, error) {
		return a.g.dispatcher.Publish(a.worldName, path, body, token, expectedVersion, meta)
	})
	if err != nil {
		return merge.PublishResult{}, err
	}
	v, vErr := optionalIntMeta(r.Response.Metadata, "version")
	if vErr != nil {
		return merge.PublishResult{}, vErr
	}
	sv, svErr := optionalIntMeta(r.Response.Metadata, "server-version")
	if svErr != nil {
		return merge.PublishResult{}, svErr
	}
	return merge.PublishResult{
		Status:        r.Response.Status,
		Version:       v,
		ServerVersion: sv,
		Metadata:      r.Response.Metadata,
	}, nil
}

// mergeDocFromResult lifts a fetch.Result into the merge
// package's Doc type, extracting the integer version from
// response metadata.
func mergeDocFromResult(r fetch.Result) (merge.Doc, error) {
	v, err := optionalIntMeta(r.Response.Metadata, "version")
	if err != nil {
		return merge.Doc{}, err
	}
	return merge.Doc{
		Status:  r.Response.Status,
		Body:    r.Response.Body,
		Version: v,
	}, nil
}

// optionalIntMeta parses meta[key] as an int. Missing or empty
// returns (0, nil) — the natural sentinel for "absent version."
// A present but malformed value returns a wrapped parse error
// so the caller surfaces server-side corruption rather than
// silently treating it as 0. Named `optionalIntMeta` (not
// `optionalInt`) to avoid colliding with any future shared
// helper in the broker package.
func optionalIntMeta(meta map[string]string, key string) (int, error) {
	s, ok := meta[key]
	if !ok || s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("response metadata %q = %q: %w", key, s, err)
	}
	return n, nil
}

// formatMergeOutcome renders a merge.Candidate result in the
// same text shape the local demarkus-mcp uses. OutcomeOK
// delegates to formatToolResult so the success path is
// byte-for-byte identical to a plain publish with
// on_conflict="fail" — the merge default doesn't change the
// success contract, only the conflict contract. OutcomeCandidate
// emits the structured envelope agents key off of:
// "status: merge-candidate" header, the three version keys, the
// has-markers flag, then a blank line and the merge candidate
// body (with or without git-style conflict markers).
func formatMergeOutcome(o *merge.Outcome) string {
	switch o.Status {
	case merge.OutcomeOK:
		return formatToolResult(fetch.Result{
			Response: protocol.Response{
				Status:   o.Publish.Status,
				Metadata: o.Publish.Metadata,
			},
		}, "version", "modified", "server-version")
	case merge.OutcomeCandidate:
		var b strings.Builder
		b.WriteString("status: merge-candidate\n")
		fmt.Fprintf(&b, "your-version: %d\n", o.BaseVersion)
		fmt.Fprintf(&b, "current-version: %d\n", o.TheirVersion)
		fmt.Fprintf(&b, "publish-at-version: %d\n", o.PublishAtVersion)
		fmt.Fprintf(&b, "has-markers: %t\n", o.HasMarkers)
		b.WriteString("\n")
		b.WriteString(o.Body)
		return b.String()
	}
	// Defensive: a future OutcomeStatus added to client/merge
	// must update this switch. Empty string is the safest
	// fallback — produces an empty tool result the agent can
	// see as obviously-wrong rather than crash-or-truncate.
	return ""
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
