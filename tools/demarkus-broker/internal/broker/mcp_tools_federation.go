package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/client/index"
	"github.com/latebit/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcp_tools_federation.go ships the federation READ tools for
// Slice 4a of the broker MCP gateway plan: `mark_discover` and
// `mark_resolve`. Both reuse dispatchWithAuth so the
// propagation-race retry + cache-hit-401 invalidation built in
// Slice 2 work here too.
//
// Slice 4b (deferred): `mark_backlinks` and `mark_graph`. Those
// need a broker-side graph store and additional client/internal
// hoists; the design question (persistent vs ephemeral vs
// defer-to-local) is intentionally left open for that follow-up.
//
// Cross-knowledge-system resolution is out of scope for Slice 4a.
// A `mark_resolve` index entry pointing at a server URL the
// broker has no WorldConfig for surfaces as a per-candidate
// `not a knowledge-system world` log line; resolution continues
// with the next candidate.

// handleMarkDiscover implements the mark_discover tool. The tool
// is a thin wrapper around `mark_fetch /.well-known/agent-manifest.md`
// — every world serves its agent manifest at the protocol's
// well-known path. Surface kept distinct (rather than the agent
// just calling mark_fetch) because the agent's prompt-side
// discovery convention is "ask the server what you can do" and
// that's easier to express as a dedicated verb.
func (g *mcpGateway) handleMarkDiscover(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, _, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	result, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.Fetch(worldName, protocol.WellKnownManifestPath, token)
	})
	if err != nil {
		return g.toolErrorFor("discover", worldName, err), nil
	}
	// Same metadata keys the local demarkus-mcp surfaces for
	// discover (version + modified). The full manifest body is
	// also part of the formatted text per formatToolResult's
	// convention.
	return mcp.NewToolResultText(formatToolResult(result, "version", "modified")), nil
}

// handleMarkResolve implements the mark_resolve tool: fetch a
// hash index document, find entries matching the requested hash,
// and try each candidate server for the actual content. The
// candidate's server URL is parsed as a `mark://{worldName}` ref
// and dispatched through the same worldDispatcher every other
// tool uses — so a candidate that isn't in the broker's
// WorldConfig surfaces as errWorldNotFound and we skip to the
// next candidate. Cross-org resolution is out of scope for
// Slice 4a; an index that points exclusively at outside servers
// returns the descriptive "could not resolve" tool error.
func (g *mcpGateway) handleMarkResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	hash, err := req.RequireString("hash")
	if err != nil {
		return mcp.NewToolResultError("hash is required"), nil
	}
	if _, ok := protocol.IsHashPath(hash); !ok {
		return mcp.NewToolResultError("invalid hash format: expected sha256-<64 lowercase hex characters>"), nil
	}
	indexURL, err := req.RequireString("index")
	if err != nil {
		return mcp.NewToolResultError("index is required"), nil
	}
	indexWorld, indexPath, err := parseToolURL(indexURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid index URL: %v", err)), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}

	// 1. Fetch the index document.
	indexResult, err := g.dispatchWithAuth(ctx, claims, indexWorld, func(token string) (fetch.Result, error) {
		return g.dispatcher.Fetch(indexWorld, indexPath, token)
	})
	if err != nil {
		return g.toolErrorFor("resolve (index fetch)", indexWorld, err), nil
	}
	if indexResult.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("index fetch returned: %s", indexResult.Response.Status)), nil
	}

	// 2. Parse and filter for matching hash entries.
	entries := index.Parse(indexResult.Response.Body)
	matches := make([]index.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Hash == hash {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("hash %s not found in index", hash)), nil
	}

	// 3. Try each candidate server. The first successful fetch
	// whose content-hash matches wins; everything else is a per-
	// candidate skip with a logged reason. The final aggregate
	// "could not resolve" message names the last failure so an
	// operator debugging a hung resolve sees the most-recent
	// candidate's complaint.
	var lastErr string
	for _, m := range matches {
		result, perCandidateErr := g.resolveCandidate(ctx, claims, m, hash)
		if perCandidateErr != "" {
			lastErr = perCandidateErr
			continue
		}
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "content-hash")), nil
	}
	return mcp.NewToolResultError(fmt.Sprintf("could not resolve hash from any server: %s", lastErr)), nil
}

// resolveCandidate attempts to fetch the content for a single
// index entry. Returns the fetch.Result on success and an empty
// error string; on any failure mode returns the canned error
// string the outer loop reports back. Split out of
// handleMarkResolve so the per-candidate decision tree (parse
// server URL → dispatch → status check → content-hash verify)
// stays readable.
func (g *mcpGateway) resolveCandidate(ctx context.Context, claims Claims, entry index.Entry, hash string) (result fetch.Result, skipReason string) {
	candidateWorld, _, perr := parseToolURL(entry.Server)
	if perr != nil {
		return fetch.Result{}, fmt.Sprintf("%s: invalid server URL: %v", entry.Server, perr)
	}
	result, derr := g.dispatchWithAuth(ctx, claims, candidateWorld, func(token string) (fetch.Result, error) {
		return g.dispatcher.Fetch(candidateWorld, "/"+hash, token)
	})
	if derr != nil {
		var notFound *errWorldNotFound
		// Both errWorldNotFound (world isn't in cfg.Worlds) and
		// ErrNotAuthorized (world is in cfg.Worlds but the
		// identity's AllowConfig denied the write) collapse to
		// "this broker can't reach
		// this candidate for me" from the agent's perspective.
		// Different distinct internal causes — same user-facing
		// recovery (try the next candidate, or accept that
		// resolution failed). Slice 4a scope: surface cleanly
		// so the agent understands the skip rather than seeing
		// a noisy raw broker error.
		if errors.As(derr, &notFound) || errors.Is(derr, ErrNotAuthorized) {
			return fetch.Result{}, fmt.Sprintf("%s: not a knowledge-system world (broker has no config or no authorized access for this server)", entry.Server)
		}
		return fetch.Result{}, fmt.Sprintf("%s: %v", entry.Server, derr)
	}
	if result.Response.Status != protocol.StatusOK {
		return fetch.Result{}, fmt.Sprintf("%s: %s", entry.Server, result.Response.Status)
	}
	if got := result.Response.Metadata["content-hash"]; got != hash {
		// The world served something at this hash path, but its
		// content-hash header doesn't match. Could be content
		// drift, hash collision (vanishingly unlikely with
		// SHA-256), or a misbehaving server. Skip — the next
		// candidate may be honest.
		return fetch.Result{}, fmt.Sprintf("%s: hash mismatch (got %s)", entry.Server, got)
	}
	return result, ""
}

// Compile-time guards: the federation handlers conform to mcp-go's
// ToolHandlerFunc shape. Catches a signature regression at build
// time, same pattern as the Slice 3 write handlers.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkDiscover
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkResolve
