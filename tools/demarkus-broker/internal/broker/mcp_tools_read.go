package broker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
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
	// The supported shape is `mark://{worldName}/{path}` —
	// double-slash, world name in the Host position. A triple
	// slash (`mark:///foo`) leaves Host="" and Path="/foo" and
	// must be rejected: there is no world name, and silently
	// re-interpreting the first path segment as a world would
	// surprise agents who typo'd the URL.
	//
	// Hostname() strips port + userinfo. We do NOT want to
	// silently swallow either: `mark://team-a:7000/foo` and
	// `mark://eve@team-a/foo` are operator typos or, worse, an
	// agent embedding credentials in a URL that the broker
	// would then drop on the floor while still issuing the
	// request as `team-a`. The broker addresses worlds by name
	// only — there is no port in this URL shape — so reject
	// outright.
	worldName = u.Hostname()
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

// worldOp is the closure form of a dispatcher call. The handler
// captures the per-verb args (path, body, expectedVersion, meta)
// in its closure; the retry loop only knows about token. This
// keeps dispatchWithAuth verb-agnostic — the write verbs (publish,
// append, archive) and the federation/graph tools share the same
// auth-race machinery without an enum + switch threaded through
// every call site. The three core read verbs (fetch, list,
// versions) dispatch unauthenticated and skip this path entirely
// — reads are open to any SSO-authed caller.
type worldOp func(token string) (fetch.Result, error)

// dispatchWithAuth resolves the per-world write token and invokes
// op against it, retrying the SAME token on `unauthorized`
// responses to absorb the one-time kubelet propagation lag the
// first time a fresh world is provisioned. The claims argument is
// kept on the signature for callsite stability but no longer
// drives the mint — there is one long-lived write token per
// world, persisted in a broker-namespace Secret, shared across
// every authorized writer. Writer authorization is enforced at
// the handler boundary (see write handlers in mcp_tools_write.go)
// before dispatchWithAuth is invoked at all.
//
// Retry budget: FirstMintMaxAttempts bounds the total attempts
// (initial + retries) against a single token. After the very
// first Provision call lands on a fresh world, kubelet projects
// the new Secret within seconds-to-minutes; subsequent dispatches
// to the same world hit the cache and return on the first call.
//
// All non-`unauthorized` responses (ok / not-modified / not-found /
// archived / not-permitted / conflict / bad-request / server-error)
// are forwarded verbatim per the byte-for-byte proxy contract.
// Transport errors short-circuit immediately.
func (g *mcpGateway) dispatchWithAuth(ctx context.Context, _ *Claims, worldName string, op worldOp) (fetch.Result, error) {
	mcpCfg := g.srv.cfg.Server.MCP
	maxAttempts := mcpCfg.FirstMintMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := mcpCfg.FirstMintInitialBackoff

	tok, err := g.srv.worldWriteTokens.Provision(ctx, worldName)
	if err != nil {
		return fetch.Result{}, err
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
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
	}
	return fetch.Result{}, fmt.Errorf("broker: world %s rejected write token after %d attempts (token propagation lag exceeded broker deadline)", worldName, maxAttempts)
}

// handleMarkFetch lives in mcp_tools_fetchmode.go together with the
// outline/section/dedup ergonomics it carries.

// handleMarkList implements the mark_list tool. Reads dispatch
// unauthenticated: the world's tokens.toml is write-only (no `read`
// operation is granted to any token), so an empty bearer flows through
// and the listing is returned. No token, no mint, no propagation-race
// retry — that machinery exists solely for writes.
func (g *mcpGateway) handleMarkList(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	opts := fetch.ListOptions{IncludeArchived: req.GetBool("include_archived", false)}
	result, err := g.dispatcher.List(worldName, path, "", opts)
	if err != nil {
		return g.toolErrorFor("list", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "modified")), nil
}

// handleMarkVersions implements the mark_versions tool. Reads
// dispatch unauthenticated; see handleMarkFetch for the rationale.
func (g *mcpGateway) handleMarkVersions(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	result, err := g.dispatcher.Versions(worldName, path, "")
	if err != nil {
		return g.toolErrorFor("versions", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "total", "current", "chain-valid", "chain-error")), nil
}

// handleMarkLookup implements the mark_lookup tool. Like the other
// reads it dispatches unauthenticated (see handleMarkFetch); the
// world ranks and filters, and the broker forwards the importance-
// ranked table verbatim. query is required; filter and limit are
// optional and passed through to the world.
func (g *mcpGateway) handleMarkLookup(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	worldName, scope, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	opts := fetch.LookupOptions{
		Filter: req.GetString("filter", ""),
		Limit:  req.GetInt("limit", 0),
	}
	result, err := g.dispatcher.Lookup(worldName, scope, query, "", opts)
	if err != nil {
		return g.toolErrorFor("lookup", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "matches")), nil
}

// toolErrorFor renders a tool-error envelope from a dispatcher
// failure. errWorldNotFound and ErrNotAuthorized /
// ErrEmailUnverified are surfaced with descriptive messages so
// the agent sees the cause; other errors fall through to a
// generic "verb failed" wrapper.
func (g *mcpGateway) toolErrorFor(verb, worldName string, err error) *mcp.CallToolResult {
	var notFound *errWorldNotFound
	if errors.As(err, &notFound) {
		return mcp.NewToolResultError(notFound.Error())
	}
	if errors.Is(err, ErrNotAuthorized) {
		return mcp.NewToolResultError(fmt.Sprintf("not authorized for world %q", worldName))
	}
	if errors.Is(err, ErrEmailUnverified) {
		return mcp.NewToolResultError("identity email is not verified")
	}
	return mcp.NewToolResultError(fmt.Sprintf("%s failed: %v", verb, err))
}

// formatToolResult mirrors the local client/cmd/demarkus-mcp
// formatResult helper byte-for-byte: status line, then the
// explicitly-named metadata keys in order, then any remaining
// metadata keys (sorted for determinism), then a blank line and
// the body (if present). Duplicated rather than hoisted into a
// shared package because:
//
//   - the function is short and stable;
//   - hoisting would touch the client module again on top of
//     Pre-Flight 0;
//   - drift is detected by the proxy-fidelity test (Slice 2 test)
//     that pins broker-side output against a verbatim copy of the
//     local server's helper.
//
// If the formatter ever does drift, the right answer is to hoist
// to client/mcpfmt at that time, not pre-emptively.
//
// The remaining-keys order is sorted (not insertion-order — Go's
// map iteration is randomized — and not a separate "extras"
// ordering — that would surprise agents diffing tool output across
// calls). Sorting is the cheapest determinism guarantee that keeps
// the local server's formatter in lockstep with this one.
func formatToolResult(r fetch.Result, keys ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", r.Response.Status)
	shown := make(map[string]bool, len(keys))
	for _, key := range keys {
		if v, ok := r.Response.Metadata[key]; ok {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
			shown[key] = true
		}
	}
	remaining := make([]string, 0, len(r.Response.Metadata))
	for k := range r.Response.Metadata {
		if !shown[k] {
			remaining = append(remaining, k)
		}
	}
	sort.Strings(remaining)
	for _, k := range remaining {
		fmt.Fprintf(&b, "%s: %s\n", k, r.Response.Metadata[k])
	}
	if r.Response.Body != "" {
		b.WriteString("\n")
		b.WriteString(r.Response.Body)
	}
	return b.String()
}
