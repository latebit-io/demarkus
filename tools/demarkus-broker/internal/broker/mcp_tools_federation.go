package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcp_tools_federation.go ships the federation READ tools `mark_discover`
// and `mark_resolve`, dispatching unauthenticated like every read.
// Cross-knowledge-system resolution is out of scope: unknown servers skip.

// handleMarkDiscover implements the mark_discover tool. The tool
// is a thin wrapper around `mark_fetch /.well-known/agent-manifest.md`
// — every world serves its agent manifest at the protocol's
// well-known path. Surface kept distinct (rather than the agent
// just calling mark_fetch) because the agent's prompt-side
// discovery convention is "ask the server what you can do" and
// that's easier to express as a dedicated verb.
func (g *mcpGateway) handleMarkDiscover(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, _, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	result, err := g.dispatcher.Fetch(worldName, protocol.WellKnownManifestPath, "")
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
func (g *mcpGateway) handleMarkResolve(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	hash, err := req.RequireString("hash")
	if err != nil {
		return mcp.NewToolResultError("hash is required"), nil
	}
	cleanHash, ok := protocol.IsHashPath(hash)
	if !ok {
		return mcp.NewToolResultError("invalid hash format: expected sha256-<64 lowercase hex characters>"), nil
	}
	hash = cleanHash
	indexURL, err := req.RequireString("index")
	if err != nil {
		return mcp.NewToolResultError("index is required"), nil
	}
	indexWorld, indexPath, err := parseToolURL(indexURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid index URL: %v", err)), nil
	}

	// 1. Fetch the index document.
	indexResult, err := g.dispatcher.Fetch(indexWorld, indexPath, "")
	if err != nil {
		return g.toolErrorFor("resolve (index fetch)", indexWorld, err), nil
	}
	if indexResult.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("index fetch returned: %s", indexResult.Response.Status)), nil
	}

	// 2. V2 manifests fetch only matching prefix shards; legacy indexes stay inline.
	matches, err := index.EntriesForHash(indexPath, indexResult.Response.Body, hash, func(shardPath string) (protocol.Response, error) {
		result, err := g.dispatcher.Fetch(indexWorld, shardPath, "")
		return result.Response, err
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid index: %v", err)), nil
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
		result, perCandidateErr := g.resolveCandidate(m, hash)
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
func (g *mcpGateway) resolveCandidate(entry index.Entry, hash string) (result fetch.Result, skipReason string) {
	candidateWorld, _, perr := parseToolURL(entry.Server)
	if perr != nil {
		return fetch.Result{}, fmt.Sprintf("%s: invalid server URL: %v", entry.Server, perr)
	}
	result, derr := g.dispatcher.Fetch(candidateWorld, "/"+hash, "")
	if derr != nil {
		var notFound *errWorldNotFound
		// A candidate the broker has no WorldConfig for is a clean skip
		// (try the next candidate), not a noisy raw broker error.
		if errors.As(derr, &notFound) {
			return fetch.Result{}, fmt.Sprintf("%s: not a knowledge-system world (broker has no config for this server)", entry.Server)
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
