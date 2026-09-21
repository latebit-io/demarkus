package broker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// crawlFetchFn returns graphstore.CrawlAndPersist's best-effort fetchFunc
// (per-fetch failures mark the node "error"). In tenant mode the crawl
// never leaves the caller's world; a foreign link errors instead of fetching.
func (g *mcpGateway) crawlFetchFn(ctx context.Context) graphstore.FetchFunc {
	tenant := ""
	if g.profile.TenantScoped {
		w, err := g.tenantWorld(ctx)
		if err != nil {
			// The tenantGate already denied unresolvable identities;
			// deny every fetch rather than crawl unscoped.
			return func(context.Context, links.Target) (graph.FetchResult, error) {
				return graph.FetchResult{}, ErrNotAuthorized
			}
		}
		tenant = w.Name
	}
	return func(ctx context.Context, target links.Target) (graph.FetchResult, error) {
		worldName := target.Hostname()
		if tenant != "" && worldName != tenant {
			return graph.FetchResult{}, fmt.Errorf("world %q is outside your world %q", worldName, tenant)
		}
		source, ok := g.worldSource(worldName, target.Path)
		if !ok {
			return graph.FetchResult{}, fmt.Errorf("unknown graph source world %q", worldName)
		}
		result, derr := g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: worldName, Path: target.Path})
		if derr != nil {
			return graph.FetchResult{}, derr
		}
		return graph.FetchResult{
			Source:   source,
			Status:   result.Response.Status,
			Body:     result.Response.Body,
			Metadata: result.Response.Metadata,
		}, nil
	}
}

// Seed checks and graph writes use the same scope selected by the handler.
func (g *mcpGateway) seedGraphStore(ctx context.Context, state *gatewayGraph) {
	worlds := g.scopedWorlds(ctx)
	for j := range worlds {
		if ctx.Err() != nil {
			return
		}
		g.seedWorldGraph(ctx, state, worlds[j].Name)
	}
}

// seedWorldGraph seeds one world's published graph into the caller's scope.
// Rows are translated to world names, then filtered to the tenant's own.
func (g *mcpGateway) seedWorldGraph(ctx context.Context, state *gatewayGraph, worldName string) {
	state.graphStore.Seed(ctx, graphstore.SeedSource{
		Owner: worldName,
		Fetch: func(ctx context.Context, path, ifNoneMatch string) (protocol.Response, error) {
			result, err := g.dispatcher.Fetch(ctx, fetch.FetchRequest{Host: worldName, Path: path, IfNoneMatch: ifNoneMatch})
			return result.Response, err
		},
		Rewrite: func(nodes []graphstore.StoredNode, edges []graphstore.StoredEdge) ([]graphstore.StoredNode, []graphstore.StoredEdge) {
			g.translateSeedURLs(nodes, edges)
			return state.ownedRows(nodes, edges)
		},
		Problem: func(p graphstore.SeedProblem) {
			g.log.Warn("graph seed failed", "world", p.Owner, "step", p.Step, "path", p.Path, "status", p.Status, "err", p.Err)
		},
	})
}

// A tenant snapshot cannot introduce another world's source rows, even if
// it aggregates several worlds. Destinations remain labels from owned sources.
func (state *gatewayGraph) ownedRows(nodes []graphstore.StoredNode, edges []graphstore.StoredEdge) ([]graphstore.StoredNode, []graphstore.StoredEdge) {
	if state.tenant == "" {
		return nodes, edges
	}
	owned := func(raw string) bool {
		world, _, err := parseToolURL(links.CanonicalURL(raw))
		return err == nil && world == state.tenant
	}
	nodes = slices.DeleteFunc(nodes, func(n graphstore.StoredNode) bool { return !owned(n.URL) })
	edges = slices.DeleteFunc(edges, func(e graphstore.StoredEdge) bool { return !owned(e.From) })
	return nodes, edges
}

// translateSeedURLs rewrites world dial addresses to mark://{worldName}/...;
// gateway queries key on world names, so untranslated rows are unreachable.
// Unknown hosts stay as labels. Both sides canonicalize (ADR 0005).
func (g *mcpGateway) translateSeedURLs(nodes []graphstore.StoredNode, edges []graphstore.StoredEdge) {
	worlds := g.srv.cfg.AllWorlds()
	byAddr := make(map[string]string, len(worlds))
	for i := range worlds {
		// CanonicalURL normalizes an empty path to "/"; the prefix match wants a
		// bare authority, so trim it back off.
		addr := strings.TrimSuffix(links.CanonicalURL("mark://"+resolveWorldAddress(&worlds[i])), "/")
		byAddr[addr] = "mark://" + worlds[i].Name
	}
	translate := func(rawURL string) string {
		canon := links.CanonicalURL(rawURL)
		for prefix, world := range byAddr {
			if rest, ok := strings.CutPrefix(canon, prefix); ok && (rest == "" || rest[0] == '/') {
				return world + rest
			}
		}
		return canon
	}
	for i := range nodes {
		// Source remains a logical authority even when the graph URL is an alias.
		if nodes[i].Observation.Source != "" {
			nodes[i].Observation.Source = links.CanonicalURL(nodes[i].Observation.Source)
		}
		nodes[i].URL = translate(nodes[i].URL)
		if nodes[i].Observation.Source != "" && translate(nodes[i].Observation.Source) != nodes[i].URL {
			nodes[i].Observation = graph.Observation{Problem: "source-mismatch"}
		}
	}
	for i := range edges {
		edges[i].From = translate(edges[i].From)
		edges[i].To = translate(edges[i].To)
	}
}

func (g *mcpGateway) handleMarkBacklinks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Backlinks(ctx, raw) })
}

func (g *mcpGateway) handleMarkGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	// An explicit 0 is the shallowest crawl here, not the default depth.
	args := marktools.GraphArgs{URL: raw, Depth: max(1, req.GetInt("depth", 2))}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Graph(ctx, args) })
}

func (g *mcpGateway) handleMarkGraphExport(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	return g.run(func(t *marktools.Tools) marktools.Result { return t.GraphExport(ctx) })
}

// defaultGraphRetention bounds the published graph document's version
// history — mirrors the local demarkus-mcp default. The graph is a generated
// artifact republished wholesale on every run, so unbounded history is pure
// growth; 20 versions is enough to debug a bad crawl.
const defaultGraphRetention = 20

// Graph publication is single-writer: version conflicts surface without merging.
func (g *mcpGateway) handleMarkGraphPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	args := marktools.GraphPublishArgs{URL: raw, Retention: req.GetInt("retention", defaultGraphRetention)}
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.GraphPublish(ctx, args) })
}

// handleMarkIndex answers mark_index. The shared body authorizes the publish
// target before the first request of the crawl; a dry run asks nobody.
func (g *mcpGateway) handleMarkIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	sourceURL, err := req.RequireString("source")
	if err != nil {
		return mcp.NewToolResultError("source is required"), nil
	}
	targetURL, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError("target is required"), nil
	}
	args := marktools.IndexArgs{
		Source: sourceURL, Target: targetURL,
		DryRun:          req.GetBool("dry_run", false),
		Force:           req.GetBool("force", false),
		ExpectedVersion: req.GetInt("expected_version", 0),
	}
	return g.run(func(t *marktools.Tools) marktools.Result { return t.Index(ctx, args) })
}
