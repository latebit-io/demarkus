package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// tenantWorldCtxKey carries the world tenantGate already resolved, so
// downstream surfaces (graph seed/crawl, mark_worlds) skip re-scanning
// the world set two or three times per request.
type tenantWorldCtxKey struct{}

func ctxWithTenantWorld(ctx context.Context, w *WorldConfig) context.Context {
	return context.WithValue(ctx, tenantWorldCtxKey{}, *w)
}

// tenantWorld resolves the caller's single world; shared by every
// tenant-gated surface. Failures log here, once per resolution, and the
// client only sees opaque error text (world names identify tenants).
func (g *mcpGateway) tenantWorld(ctx context.Context) (WorldConfig, error) {
	if w, ok := ctx.Value(tenantWorldCtxKey{}).(WorldConfig); ok {
		return w, nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return WorldConfig{}, ErrNotAuthorized
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
		return WorldConfig{}, err
	}
	return w, nil
}

// resolveOrProvision resolves the caller's world, provisioning one on
// first arrival when the broker runs with dynamic provisioning. The
// second return is a ready-to-return tool error when resolution fails.
func (g *mcpGateway) resolveOrProvision(ctx context.Context) (WorldConfig, *mcp.CallToolResult) {
	w, err := g.tenantWorld(ctx)
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, ErrNotAuthorized) || g.srv.provisioner == nil {
		return WorldConfig{}, mcp.NewToolResultError(fmt.Sprintf("not authorized: %v", err))
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return WorldConfig{}, mcp.NewToolResultError("internal: missing identity on tool-call context")
	}
	w, perr := g.srv.provisioner.EnsureTenant(ctx, claims)
	switch {
	case perr == nil:
		// The knowledge server picks the new world up asynchronously;
		// give the caller a clear retry message until it answers.
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, ferr := g.dispatcher.FetchContext(probeCtx, w.Name, "/index.md", "")
		cancel()
		if ferr != nil {
			return WorldConfig{}, mcp.NewToolResultError("your memory world is being provisioned; try again in about a minute")
		}
		return w, nil
	case errors.Is(perr, ErrProvisioningDenied):
		g.log.Warn("tenant provisioning denied by gate", "subject", hashSubject(claims.Subject))
		return WorldConfig{}, mcp.NewToolResultError("not authorized: this memory service does not admit your identity; contact the operator")
	case errors.Is(perr, ErrTenantCapacity):
		g.log.Warn("tenant provisioning denied at capacity", "subject", hashSubject(claims.Subject))
		return WorldConfig{}, mcp.NewToolResultError("this memory service is at capacity; contact the operator")
	default:
		g.log.Warn("tenant provisioning failed", "subject", hashSubject(claims.Subject), "err", perr)
		return WorldConfig{}, mcp.NewToolResultError("provisioning your memory world failed; try again shortly")
	}
}

// tenantGate (tenant-scoped profiles only) resolves the caller's world,
// checks every world-bearing argument against it, and seeds the soul
// template before the first handler runs against a fresh world.
func (g *mcpGateway) tenantGate(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		w, errRes := g.resolveOrProvision(ctx)
		if errRes != nil {
			return errRes, nil
		}
		args, known := tenantWorldArgs[req.Params.Name]
		if !known {
			// Deny closed: an unclassified tool must never dispatch in
			// tenant mode.
			g.log.Warn("tenant gate denied unclassified tool", "tool", req.Params.Name, "world", w.Name)
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
				// Cross-tenant attempts are the security signal; the
				// target world stays out of the log too.
				g.log.Warn("tenant gate denied cross-tenant access", "tool", req.Params.Name, "world", w.Name)
				return mcp.NewToolResultError(fmt.Sprintf("access denied: this memory service serves only your world %q", w.Name)), nil
			}
		}
		ctx = ctxWithTenantWorld(ctx, &w)
		if g.profile.SeedSoul {
			g.ensureSoulSeed(ctx, &w)
		}
		return next(ctx, req)
	}
}

// scopedWorlds returns the worlds visible to the current call: in
// tenant mode the caller's single world (empty on resolution failure,
// degrading closed), otherwise every configured world.
func (g *mcpGateway) scopedWorlds(ctx context.Context) []WorldConfig {
	if !g.profile.TenantScoped {
		return readableWorlds(g.srv.cfg)
	}
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return nil
	}
	return []WorldConfig{w}
}
