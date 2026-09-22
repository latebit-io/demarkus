package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/broker/storage"
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

func ctxWithTenantWorld(ctx context.Context, w *core.WorldConfig) context.Context {
	return context.WithValue(ctx, tenantWorldCtxKey{}, *w)
}

// tenantWorld resolves the caller's single world; shared by every
// tenant-gated surface. Failures log here, once per resolution, and the
// client only sees opaque error text (world names identify tenants).
func (g *Gateway) tenantWorld(ctx context.Context) (core.WorldConfig, error) {
	if w, ok := ctx.Value(tenantWorldCtxKey{}).(core.WorldConfig); ok {
		return w, nil
	}
	claims, ok := core.ClaimsFromCtx(ctx)
	if !ok {
		// Auth middleware attaches claims before any handler; absence is
		// an internal invariant break worth its own trace.
		g.deps.Log.Warn("tenant world requested without identity on context")
		return core.WorldConfig{}, core.ErrNotAuthorized
	}
	w, err := core.TenantWorldFor(g.deps.Worlds, g.deps.Issuer, claims)
	if err != nil {
		g.evictTenantGraphs(core.IdentityKey(g.deps.Issuer, claims.Subject))
		var ambiguous core.AmbiguousTenantError
		if errors.As(err, &ambiguous) {
			g.deps.Log.Warn("tenant resolution ambiguous; denying closed",
				"subject", core.HashSubject(claims.Subject), "first", ambiguous.First, "second", ambiguous.Second)
		} else {
			g.deps.Log.Warn("tenant resolution failed", "subject", core.HashSubject(claims.Subject), "err", err)
		}
		return core.WorldConfig{}, err
	}
	return w, nil
}

// admitTenant is the one door into tenant mode, for tool calls and resource
// reads: the caller's world, provisioned on first arrival, carried on ctx.
// The error is client text; causes are logged (world names name tenants).
func (g *Gateway) admitTenant(ctx context.Context) (context.Context, core.WorldConfig, error) {
	w, err := g.resolveOrProvision(ctx)
	if err != nil {
		return ctx, core.WorldConfig{}, err
	}
	ctx = ctxWithTenantWorld(ctx, &w)
	return ctx, w, nil
}

// seedTenant gives a fresh world its memory template. It runs after the one
// world rule, so a refused call writes nothing.
func (g *Gateway) seedTenant(ctx context.Context, w *core.WorldConfig) {
	if g.profile.SeedMemory {
		g.ensureMemorySeed(ctx, w)
	}
}

// resolveOrProvision resolves the caller's world, provisioning one on first
// arrival when the broker provisions dynamically.
func (g *Gateway) resolveOrProvision(ctx context.Context) (core.WorldConfig, error) {
	w, err := g.tenantWorld(ctx)
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, core.ErrNotAuthorized) || g.deps.Provisioner == nil {
		return core.WorldConfig{}, fmt.Errorf("not authorized: %w", err)
	}
	claims, ok := core.ClaimsFromCtx(ctx)
	if !ok {
		return core.WorldConfig{}, errors.New("internal: missing identity on tool-call context")
	}
	identity := core.IdentityKey(g.deps.Issuer, claims.Subject)
	if refusal := g.refusals.recent(identity, g.deps.Clock()); refusal != nil {
		return core.WorldConfig{}, refusal
	}
	w, perr := g.deps.Provisioner.EnsureTenant(ctx, claims)
	if perr == nil {
		// The knowledge server picks the new world up asynchronously;
		// give the caller a clear retry message until it answers.
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, ferr := g.dispatcher.Fetch(probeCtx, fetch.FetchRequest{Host: w.Name, Path: "/index.md"})
		cancel()
		if ferr != nil {
			return core.WorldConfig{}, errors.New("your memory world is being provisioned; try again in about a minute")
		}
		return w, nil
	}
	remember, refusal := g.provisioningRefusal(claims, perr)
	if remember {
		g.refusals.remember(identity, refusal, g.deps.Clock())
	}
	return core.WorldConfig{}, refusal
}

// provisioningRefusal words a failed provisioning for the client and logs its
// cause. remember is true for an answer that will not change within a minute;
// a transient failure is retried on the next call.
func (g *Gateway) provisioningRefusal(claims *core.Claims, err error) (remember bool, refusal error) {
	subject := core.HashSubject(claims.Subject)
	switch {
	case errors.Is(err, storage.ErrProvisioningDenied):
		g.deps.Log.Warn("tenant provisioning denied by gate", "subject", subject)
		return true, errors.New("not authorized: this memory service does not admit your identity; contact the operator")
	case errors.Is(err, storage.ErrTenantDeprovisioning):
		g.deps.Log.Warn("tool call denied for tombstoned tenant", "subject", subject)
		return true, errors.New("your memory world is being removed; contact the operator if this persists")
	case errors.Is(err, storage.ErrTenantCapacity):
		g.deps.Log.Warn("tenant provisioning denied at capacity", "subject", subject)
		return true, errors.New("this memory service is at capacity; contact the operator")
	}
	g.deps.Log.Warn("tenant provisioning failed", "subject", subject, "err", err)
	return false, errors.New("provisioning your memory world failed; try again shortly")
}

// tenantOwns is whether raw addresses the tenant's own world. A url that does
// not parse is the handler's to report, in its own words.
func tenantOwns(w *core.WorldConfig, raw string) (owns bool, parseErr error) {
	docURL, _, _ := strings.Cut(raw, "#") // section aware handlers cut it too
	worldName, _, err := parseToolURL(docURL)
	if err != nil {
		return false, err
	}
	return worldName == w.Name, nil
}

func crossTenantDenial(w *core.WorldConfig) error {
	return fmt.Errorf("access denied: this memory service serves only your world %q", w.Name)
}

// tenantGate (tenant-scoped profiles only) admits the caller, then checks every
// world-bearing argument against the caller's world before the handler runs.
func (g *Gateway) tenantGate(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, known := tenantWorldArgs[req.Params.Name]
		if !known {
			// Deny closed, and before admission: a tool that does not
			// exist here must not provision a world on its way out.
			g.deps.Log.Warn("tenant gate denied unclassified tool", "tool", req.Params.Name)
			return mcp.NewToolResultError(fmt.Sprintf("tool %q is not available on this memory broker", req.Params.Name)), nil
		}
		ctx, w, err := g.admitTenant(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		for _, name := range args {
			raw := strings.TrimSpace(req.GetString(name, ""))
			if raw == "" {
				continue // the handler's own required-arg check reports it
			}
			if owns, parseErr := tenantOwns(&w, raw); parseErr == nil && !owns {
				// Cross-tenant attempts are the security signal; the
				// target world stays out of the log too.
				g.deps.Log.Warn("tenant gate denied cross-tenant access", "tool", req.Params.Name, "world", w.Name)
				return mcp.NewToolResultError(crossTenantDenial(&w).Error()), nil
			}
		}
		g.seedTenant(ctx, &w)
		return next(ctx, req)
	}
}

// scopedWorlds returns the worlds visible to the current call: in
// tenant mode the caller's single world (empty on resolution failure,
// degrading closed), otherwise every configured world.
func (g *Gateway) scopedWorlds(ctx context.Context) []core.WorldConfig {
	if !g.profile.TenantScoped {
		return core.ReadableWorlds(g.deps.Worlds)
	}
	w, err := g.tenantWorld(ctx)
	if err != nil {
		return nil
	}
	return []core.WorldConfig{w}
}
