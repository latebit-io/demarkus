package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Tenant scoping for the memory broker: identity = world. tenantGate
// runs before every tool handler and denies cross-tenant world args;
// mcp_tenant_test.go enforces the invariant across every registered tool.

// tenantWorldArgs names, per tool, the arguments carrying a mark:// URL
// whose world must equal the caller's tenant world. An absent tool is
// denied closed: classify a new tool here before it can serve traffic.
var tenantWorldArgs = map[string][]string{
	"mark_fetch":     {"url"},
	"mark_explore":   {"url"},
	"mark_list":      {"url"},
	"mark_versions":  {"url"},
	"mark_lookup":    {"url"},
	"mark_publish":   {"url"},
	"mark_append":    {"url"},
	"mark_archive":   {"url"},
	"mark_discover":  {"url"},
	"mark_backlinks": {"url"},
	"mark_graph":     {"url"},
	"mark_worlds":    {},
}

// tenantWorld resolves the caller's single world; shared by every
// tenant-gated surface. Failures log here, once per resolution, and the
// client only sees opaque error text (world names identify tenants).
func (g *mcpGateway) tenantWorld(ctx context.Context) (*WorldConfig, error) {
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return nil, ErrNotAuthorized
	}
	w, err := tenantWorldFor(g.srv.cfg, claims)
	if err != nil {
		var ambiguous errAmbiguousTenant
		if errors.As(err, &ambiguous) {
			g.log.Warn("tenant resolution ambiguous; denying closed",
				"subject", hashSubject(claims.Subject), "first", ambiguous.First, "second", ambiguous.Second)
		} else {
			g.log.Warn("tenant resolution failed", "subject", hashSubject(claims.Subject), "err", err)
		}
		return nil, err
	}
	return w, nil
}

// tenantGate (tenant-scoped profiles only) resolves the caller's world,
// checks every world-bearing argument against it, and seeds the soul
// template before the first handler runs against a fresh world.
func (g *mcpGateway) tenantGate(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		w, err := g.tenantWorld(ctx)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("not authorized: %v", err)), nil
		}
		args, known := tenantWorldArgs[req.Params.Name]
		if !known {
			// Deny closed: an unclassified tool must never dispatch in
			// tenant mode.
			return mcp.NewToolResultError(fmt.Sprintf("tool %q is not available on this memory broker", req.Params.Name)), nil
		}
		for _, name := range args {
			raw := strings.TrimSpace(req.GetString(name, ""))
			if raw == "" {
				continue // the handler's own required-arg check reports it
			}
			worldName, _, perr := parseToolURL(raw)
			if perr != nil {
				continue // the handler's own URL validation reports it
			}
			if worldName != w.Name {
				return mcp.NewToolResultError(fmt.Sprintf("access denied: this memory service serves only your world %q", w.Name)), nil
			}
		}
		if g.profile.SeedSoul {
			g.ensureSoulSeed(ctx, w)
		}
		return next(ctx, req)
	}
}

// handleMarkWorldsSelf is the tenant-scoped mark_worlds handler: only
// ever the caller's own world, always writable. Column parity with
// handleMarkWorlds keeps client-side parsers identical across brokers.
func (g *mcpGateway) handleMarkWorldsSelf(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("not authorized: %v", err)), nil
	}
	var b strings.Builder
	b.WriteString("status: ok\ncount: 1\n")
	b.WriteString("\n| world | url | address | writable |\n|-------|-----|---------|----------|\n")
	fmt.Fprintf(&b, "| %s | %s | mark://%s | yes |\n", w.Name, w.PublicURL, resolveWorldAddress(w))
	return mcp.NewToolResultText(b.String()), nil
}

// scopedWorlds returns the worlds visible to the current call: in
// tenant mode the caller's single world (empty on resolution failure,
// degrading closed), otherwise every configured world.
func (g *mcpGateway) scopedWorlds(ctx context.Context) []*WorldConfig {
	if !g.profile.TenantScoped {
		return readableWorlds(g.srv.cfg)
	}
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return nil
	}
	return []*WorldConfig{w}
}

// compile-time check that the handler matches mcp-go's expected shape.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkWorldsSelf
