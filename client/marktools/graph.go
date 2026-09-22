package marktools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/merge"
)

// GraphScope is the graph a call works on: one store for a stdio server, the
// caller's tenant store behind the broker.
type GraphScope struct {
	Store *graphstore.Store
	// Fetch reads documents for crawls and revalidation, under the surface's
	// own routing and scoping rules.
	Fetch graphstore.FetchFunc
	// Source is where a document really lives when its NodeURL is a routing
	// alias; observations record it, as the crawl's Fetch does. Nil is NodeURL.
	Source func(target Target) string
	// Seed refreshes the store from published graphs before a query; nil skips.
	Seed func(ctx context.Context, target Target)
	// EmptyHint follows "No backlinks found": how this surface fills its store.
	EmptyHint string
}

// graphScope returns the call's scope, or the failure to answer with.
func (t *Tools) graphScope(ctx context.Context) (*GraphScope, *Result) {
	if t.hooks.Graph == nil {
		bad := failure("graph store not available")
		return nil, &bad
	}
	scope, err := t.hooks.Graph(ctx)
	if err != nil {
		bad := failure("%v", err)
		return nil, &bad
	}
	return scope, nil
}

func (s *GraphScope) seed(ctx context.Context, target Target) {
	if s.Seed != nil {
		s.Seed(ctx, target)
	}
}

// Backlinks answers mark_backlinks: who links to the document, after a
// freshness pass over those sources.
func (t *Tools) Backlinks(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	scope, bad := t.graphScope(ctx)
	if bad != nil {
		return *bad
	}
	scope.seed(ctx, target)

	var b strings.Builder
	b.WriteString(scope.Store.RevalidateBacklinks(ctx, target.NodeURL, scope.Fetch))
	backlinks := scope.Store.BacklinksEnriched(target.NodeURL)
	if len(backlinks) == 0 {
		fmt.Fprintf(&b, "No backlinks found for %s\n%s", target.NodeURL, scope.EmptyHint)
		return text(b.String())
	}
	fmt.Fprintf(&b, "Backlinks for %s (%d):\n\n", target.NodeURL, len(backlinks))
	for _, bl := range backlinks {
		ann := graph.EdgeAnnotation(bl.Rel, bl.Label, bl.Anchor, bl.Count) + bl.Observation.Annotation()
		if bl.Title != "" {
			fmt.Fprintf(&b, "- [%s](%s)%s\n", bl.Title, bl.URL, ann)
		} else {
			fmt.Fprintf(&b, "- %s%s\n", bl.URL, ann)
		}
	}
	return text(b.String())
}

// DefaultGraphDepth is the crawl depth when none is given.
const DefaultGraphDepth = 2

// GraphArgs are mark_graph's arguments; Depth is clamped to 1..5, zero is the default.
type GraphArgs struct {
	URL   string
	Depth int
}

// Graph answers mark_graph: crawl from the document and keep what was found.
func (t *Tools) Graph(ctx context.Context, args GraphArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	scope, bad := t.graphScope(ctx)
	if bad != nil {
		return *bad
	}
	depth := args.Depth
	if depth == 0 {
		depth = DefaultGraphDepth
	}
	// Seeded first, so a depth limited crawl still has the hub's context.
	scope.seed(ctx, target)
	g, err := scope.Store.CrawlAndPersist(ctx, target.NodeURL, scope.Fetch, graphstore.CrawlOptions{
		MaxDepth: max(1, min(depth, 5)),
		MaxNodes: 200,
		Workers:  5,
	})
	if err != nil && g == nil {
		return failure("crawl failed: %v", err)
	}
	out := graph.Summary(g, target.NodeURL)
	if warning := graph.CrawlWarning(err, g.Outcome); warning != nil {
		out += fmt.Sprintf("\nwarning: %v\n", warning)
	}
	return text(out)
}

// GraphExport answers mark_graph_export: the store as a graph document.
func (t *Tools) GraphExport(ctx context.Context) Result {
	scope, bad := t.graphScope(ctx)
	if bad != nil {
		return *bad
	}
	return text(scope.Store.Export())
}

// GraphPublishArgs are mark_graph_publish's arguments. Retention above zero
// asks the server to prune the graph document's history to that many versions.
type GraphPublishArgs struct {
	URL             string
	ExpectedVersion *int
	Retention       int
}

// GraphPublish answers mark_graph_publish: the export, published as a document.
func (t *Tools) GraphPublish(ctx context.Context, args GraphPublishArgs) Result {
	// An empty url would otherwise resolve to the surface's default server.
	if args.URL == "" {
		return failure("url is required")
	}
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	expected, bad := requireVersion(args.ExpectedVersion)
	if bad != nil {
		return *bad
	}
	if args.Retention < 0 {
		return failure("retention must be >= 0 (0 keeps every version)")
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		return *bad
	}
	scope, bad := t.graphScope(ctx)
	if bad != nil {
		return *bad
	}
	meta := t.agentMeta(ctx)
	if args.Retention > 0 {
		meta["retention"] = strconv.Itoa(args.Retention)
	}
	// Single writer, so a conflict is reported, never merged.
	result, err := t.doc(ctx, target, write).Publish(ctx, docwrite.Write{
		Body: scope.Store.Export(), ExpectedVersion: expected, Metadata: meta,
	}, merge.OnConflictFail)
	if err != nil {
		return t.failed(SiteGraphPublish, target.Host, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Published graph (%d nodes, %d edges) to %s\n", scope.Store.NodeCount(), scope.Store.EdgeCount(), target.NodeURL)
	b.WriteString(formatResult(&result, writeFields...))
	return text(b.String())
}
