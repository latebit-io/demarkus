package broker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/protocol"
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

// mintFuncFor returns a mintFunc bound to the given identity and
// worldName. The closure narrows the broker's Issuer.MintFiltered
// to the single target world via a keep predicate so the lazy mint
// produces exactly one issuance — minting against every authorized
// world on a tool call against one would burn Secret writes on
// worlds the user may never touch this session.
//
// The returned cachedWorldToken carries Issuer.MintFiltered's own
// ExpiresAt so the session cache evicts entries past their natural
// lifetime without round-tripping to the world.
func (g *mcpGateway) mintFuncFor(claims Claims, worldName string) mintFunc {
	return func(ctx context.Context) (cachedWorldToken, error) {
		keep := func(w *WorldConfig) bool { return w.Name == worldName }
		results, err := g.srv.issuer.MintFiltered(ctx, claims, keep)
		if err != nil {
			return cachedWorldToken{}, err
		}
		// MintFiltered with a single-world keep predicate returns
		// either zero results (filtered out — world exists but
		// identity is not authorized for it) or exactly one. Zero
		// is a real failure to surface to the caller; the
		// keep-everything case never lands here.
		if len(results) != 1 {
			return cachedWorldToken{}, fmt.Errorf("broker: mint returned %d results for world %q (expected exactly 1)", len(results), worldName)
		}
		r := results[0]
		return cachedWorldToken{raw: r.RawToken, expiresAt: r.ExpiresAt}, nil
	}
}

// readOp is the small enum the dispatcher consumes to choose
// Fetch/List/Versions on the worldDispatcher. Defined as an enum
// rather than three near-identical methods so the retry loop
// (cache hit → 401 → invalidate → re-mint → retry) stays in one
// place across all three read verbs.
type readOp int

const (
	opFetch readOp = iota
	opList
	opVersions
)

func (o readOp) String() string {
	switch o {
	case opFetch:
		return "fetch"
	case opList:
		return "list"
	case opVersions:
		return "versions"
	default:
		return "unknown"
	}
}

// dispatchRead pulls or lazy-mints the world token for (email,
// worldName), then invokes the dispatcher op. On a world response
// of `unauthorized`:
//
//   - cache hit: the token the world has changed under us
//     (operator hand-revoked, sweeper retired, world Secret reset
//     during a restore). Invalidate and immediately fall through
//     to a fresh mint — no sleep, no budget consumption.
//   - fresh mint: the world hasn't propagated the new issuance
//     yet (kubelet projected-volume refresh in flight). Back off
//     and retry up to cfg.Server.MCP.FirstMintMaxAttempts with
//     exponential backoff from FirstMintInitialBackoff to
//     FirstMintMaxBackoff. The budget is for fresh-mint
//     unauthorized responses only — a single cache-hit 401 at
//     the start of the call MUST NOT reduce the propagation-race
//     retries available afterward.
//
// All other responses (ok / not-modified / not-found / archived /
// not-permitted / conflict / bad-request / server-error) are
// forwarded verbatim to the caller per the byte-for-byte proxy
// contract. Transport errors short-circuit immediately — they're
// not auth races.
func (g *mcpGateway) dispatchRead(ctx context.Context, claims Claims, worldName, path string, op readOp) (fetch.Result, error) {
	mcpCfg := g.srv.cfg.Server.MCP
	maxAttempts := mcpCfg.FirstMintMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := mcpCfg.FirstMintInitialBackoff
	mint := g.mintFuncFor(claims, worldName)

	// freshUnauthorized counts only the propagation-race attempts:
	// fresh-mint tokens the world rejected. Cache-hit rejections
	// drop the entry and loop immediately without touching this
	// counter so a tight maxAttempts config still gives the full
	// propagation-race budget after a stale-cache wakeup.
	freshUnauthorized := 0
	for {
		tok, isFresh, err := g.sessionCache.GetOrMint(ctx, claims.Email, worldName, mint)
		if err != nil {
			return fetch.Result{}, err
		}
		result, err := g.dispatchOp(op, worldName, path, tok.raw)
		if err != nil {
			// Transport-level failure — no auth race semantics
			// apply. Return immediately; the caller maps it to
			// an MCP tool error.
			return fetch.Result{}, err
		}
		if result.Response.Status != protocol.StatusUnauthorized {
			return result, nil
		}
		// Unauthorized response. The cache entry is wrong (or
		// the world hasn't seen our newly-minted token yet).
		// Drop the cache and retry. The next iteration's
		// GetOrMint will fresh-mint, so the retry covers both
		// the "stale cache" and "kubelet propagation lag" cases.
		g.sessionCache.Invalidate(claims.Email, worldName)
		if !isFresh {
			// Cache-hit token rejected. The next iteration is
			// the fresh-mint case; loop without sleeping and
			// without counting against the propagation-race
			// budget — the cache was stale, not the world.
			continue
		}
		// Fresh-mint token rejected. Consumes the propagation-
		// race budget; bail out cleanly when exhausted so the
		// agent sees a descriptive error instead of a hung
		// tool call.
		freshUnauthorized++
		if freshUnauthorized >= maxAttempts {
			return fetch.Result{}, fmt.Errorf("broker: world %s rejected freshly-minted token after %d attempts (token propagation lag exceeded broker deadline)", worldName, maxAttempts)
		}
		// Sleep with bounded exponential backoff before the
		// next attempt. Context cancellation short-circuits so
		// a client hangup doesn't keep the broker spinning.
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
}

// dispatchOp routes to the dispatcher method matching op. Kept
// out of dispatchRead so the retry loop stays focused on the
// auth-race lifecycle.
func (g *mcpGateway) dispatchOp(op readOp, worldName, path, token string) (fetch.Result, error) {
	switch op {
	case opFetch:
		return g.dispatcher.Fetch(worldName, path, token)
	case opList:
		return g.dispatcher.List(worldName, path, token)
	case opVersions:
		return g.dispatcher.Versions(worldName, path, token)
	default:
		return fetch.Result{}, fmt.Errorf("broker: unknown read op %d", op)
	}
}

// handleMarkFetch implements the mark_fetch tool against the
// brokered world. Output format mirrors the local
// client/cmd/demarkus-mcp emission for parity (plan v5
// byte-for-byte proxy contract): formatToolResult is the
// duplicated formatter; the proxy-fidelity test pins drift.
func (g *mcpGateway) handleMarkFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
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
		// Unreachable when wired through gatewayAuth +
		// subjectRateLimit; the 500-equivalent envelope makes
		// the regression loud if a future composition skips
		// the middleware.
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	result, err := g.dispatchRead(ctx, claims, worldName, path, opFetch)
	if err != nil {
		return g.toolErrorFor("fetch", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "etag")), nil
}

// handleMarkList implements the mark_list tool. Output keys
// mirror the local server's choice (just `modified`); remaining
// metadata keys are appended to give the agent full visibility
// (formatToolResult convention).
func (g *mcpGateway) handleMarkList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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
	result, err := g.dispatchRead(ctx, claims, worldName, path, opList)
	if err != nil {
		return g.toolErrorFor("list", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "modified")), nil
}

// handleMarkVersions implements the mark_versions tool. Same
// formatToolResult key ordering as the local server so the
// proxy-fidelity test can pin equality across both surfaces.
func (g *mcpGateway) handleMarkVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
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
	result, err := g.dispatchRead(ctx, claims, worldName, path, opVersions)
	if err != nil {
		return g.toolErrorFor("versions", worldName, err), nil
	}
	return mcp.NewToolResultText(formatToolResult(result, "total", "current", "chain-valid", "chain-error")), nil
}

// toolErrorFor renders a tool-error envelope from a dispatcher
// failure. errWorldNotFound and Issuer's ErrNotAuthorized /
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
