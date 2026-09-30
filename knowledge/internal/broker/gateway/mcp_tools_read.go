package gateway

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpbind"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// parseToolURL splits a tool URL, mark://{worldName}/{path}, where the world
// name is the address the WorldPool resolves; a bare host means path "/".
// Shape errors are the agent's input problem and end in a tool error.
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

// writeContext is ctx carrying the grant the caller's identity earns on the
// world, or ErrNotAuthorized. A write sent under it needs no token.
func (g *Gateway) writeContext(ctx context.Context, worldName string) (context.Context, error) {
	claims, ok := core.ClaimsFromCtx(ctx)
	if !ok {
		return nil, core.ErrNotAuthorized
	}
	grant, err := g.writeGrantFor(claims, worldName)
	if err != nil {
		return nil, core.ErrNotAuthorized
	}
	return protocol.WithGrant(ctx, grant), nil
}

// handleMarkList answers mark_list. Reads carry no token: a world grants read
// to no token, so none is minted for one.
func (g *Gateway) handleMarkList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.List(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.List(ctx, args) })
}

// handleMarkVersions answers mark_versions.
func (g *Gateway) handleMarkVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := mcpbind.URL(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Versions(ctx, raw) })
}

// handleMarkLookup answers mark_lookup.
func (g *Gateway) handleMarkLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	args, err := mcpbind.Lookup(&req)
	if err != nil {
		return mcpbind.Refused(err), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Lookup(ctx, args) })
}
