package broker

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// parseToolURL extracts the worldName and path from a tool's URL
// argument. Tool URLs have the broker-specific shape
// `mark://{worldName}/{path}` (no host:port — the worldName IS the
// addressing primitive; the broker resolves it to a cluster-internal
// Service DNS address via worldPool). A bare hostname (no slash,
// no path) is treated as path="/" so `mark://team-a` and
// `mark://team-a/` produce the same handler input.
//
// Returns a descriptive error on shape violations so the agent
// sees a useful tool-error envelope instead of a transport-level
// failure. Errors here are USER input shape problems, never
// programming errors — every failure mode terminates in
// mcp.NewToolResultError at the handler.
func parseToolURL(raw string) (worldName, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "mark" {
		return "", "", fmt.Errorf("unsupported scheme %q (expected mark://)", u.Scheme)
	}
	// The shape is mark://{worldName}/{path}. A port or userinfo is refused
	// below, never dropped: credentials in a URL must not pass silently.
	// Host case is not identity (ADR 0018); world names are lowercase by rule.
	worldName = strings.ToLower(u.Hostname())
	path = u.Path
	if worldName == "" {
		// Also handles rootless forms (mark:/team-a/foo) which
		// url.Parse can place in Opaque. The plan v3 contract
		// is double-slash with the worldName as host; anything
		// else is a malformed tool URL.
		return "", "", fmt.Errorf("missing world name in URL %q (expected mark://{worldName}/{path})", raw)
	}
	if u.Port() != "" {
		return "", "", fmt.Errorf("invalid URL %q: world host must not carry a port (the broker resolves world names internally)", raw)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("invalid URL %q: world host must not carry userinfo", raw)
	}
	// Query strings and fragments are not part of the demarkus
	// protocol's address shape. Forwarding only u.Path would
	// silently strip them — the agent would see a success
	// response for a different target than it requested. Reject
	// so a typo'd `mark://team-a/foo.md?rev=1` surfaces the
	// shape error instead of pretending to succeed.
	if u.RawQuery != "" {
		return "", "", fmt.Errorf("invalid URL %q: query parameters are not supported", raw)
	}
	if u.Fragment != "" {
		return "", "", fmt.Errorf("invalid URL %q: fragments are not supported", raw)
	}
	if path == "" {
		path = "/"
	}
	return worldName, path, nil
}

// worldOp captures one publish operation for token-aware retry.
type worldOp func(token string) (fetch.Result, error)

// dispatchWithWriteAuth centrally enforces writer access, provisions a publish
// token, and absorbs propagation lag. Handler gates provide precise errors.
func (g *mcpGateway) dispatchWithWriteAuth(ctx context.Context, worldName string, op worldOp) (fetch.Result, error) {
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return fetch.Result{}, ErrNotAuthorized
	}
	if g.writeRefusal(claims, worldName) != nil {
		return fetch.Result{}, ErrNotAuthorized
	}
	mcpCfg := g.deps.MCP
	maxAttempts := mcpCfg.FirstMintMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := mcpCfg.FirstMintInitialBackoff

	tok, err := g.deps.WriteTokens.Provision(ctx, worldName)
	if err != nil {
		return fetch.Result{}, err
	}

	reprovisioned := false
	for attempt := range maxAttempts {
		if attempt > 0 {
			// Backoff before the retry, not before the first attempt.
			// Context cancellation short-circuits so a client hangup
			// doesn't keep the broker spinning.
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fetch.Result{}, ctx.Err()
			}
			backoff *= 2
			if backoff > mcpCfg.FirstMintMaxBackoff {
				backoff = mcpCfg.FirstMintMaxBackoff
			}
		}
		result, opErr := op(tok)
		if opErr != nil {
			// Transport-level failure — no auth race semantics
			// apply. Return immediately; the caller maps it to an
			// MCP tool error.
			return fetch.Result{}, opErr
		}
		if result.Response.Status != protocol.StatusUnauthorized {
			return result, nil
		}
		// First 401 only: invalidate + re-provision (syncWorldHash reconciles a
		// rotated world Secret). Later 401s are kubelet propagation lag, where
		// re-provisioning would re-read the same token at 2 round trips a retry.
		if !reprovisioned {
			reprovisioned = true
			g.deps.WriteTokens.Invalidate(worldName)
			if tok, err = g.deps.WriteTokens.Provision(ctx, worldName); err != nil {
				return fetch.Result{}, err
			}
		}
	}
	return fetch.Result{}, fmt.Errorf("broker: world %s rejected write token after %d attempts (token propagation lag exceeded broker deadline)", worldName, maxAttempts)
}

// handleMarkList answers mark_list. Reads carry no token: a world grants read
// to no token, so none is minted for one.
func (g *mcpGateway) handleMarkList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.ListArgs{
		URL:             raw,
		IncludeArchived: req.GetBool("include_archived", false),
		Cursor:          req.GetString("cursor", ""),
		PageSize:        req.GetArguments()["page_size"],
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.List(ctx, args) })
}

// handleMarkVersions answers mark_versions.
func (g *mcpGateway) handleMarkVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Versions(ctx, raw) })
}

// handleMarkLookup answers mark_lookup.
func (g *mcpGateway) handleMarkLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	args := marktools.LookupArgs{
		URL: raw, Query: query,
		Filter: req.GetString("filter", ""),
		Limit:  req.GetInt("limit", 0),
		Match:  req.GetString("match", ""),
		Budget: lookupexpand.Budget(&req),
		Render: mcpfmt.Lookup.Options(&req),
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Lookup(ctx, args) })
}
