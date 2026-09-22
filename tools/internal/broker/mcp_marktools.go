package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/marktools"
)

// toolBodies binds the shared tool bodies to the gateway: a url names a world,
// reads carry no token, and errors are worded for an agent behind the broker.
func (g *mcpGateway) toolBodies() (*marktools.Tools, error) {
	return marktools.New(g.dispatcher, marktools.Hooks{
		Resolve: resolveToolTarget,
		Seen:    g.fetchSeen,
		Writer:  g.toolWriter,
		Agent:   toolAgent,
		Graph:   g.toolGraph,
		Warnf:   func(format string, args ...any) { g.deps.Log.Warn(fmt.Sprintf(format, args...)) },
		ErrText: toolSiteErrorText,
	})
}

// resolveToolTarget keeps the url as the agent wrote it for display; the graph
// store and the formatters canonicalize what they key and compare.
func resolveToolTarget(_ context.Context, raw string) (marktools.Target, error) {
	worldName, path, err := parseToolURL(raw)
	if err != nil {
		return marktools.Target{}, err
	}
	return marktools.Target{Host: worldName, Path: path, NodeURL: raw, Authority: "mark://" + worldName}, nil
}

// worldSource is where a world's document really lives. Observations, crawls
// and published snapshots all record it, so the three agree on a source.
func (g *mcpGateway) worldSource(worldName, path string) (string, bool) {
	world, ok := g.deps.Worlds.Find(worldName)
	if !ok {
		return "", false
	}
	return links.NodeURL(resolveWorldAddress(&world), path), true
}

// toolWriter refuses a caller who may not write to the world, in words. The
// write it returns is gated again and carries the world's write token, with
// the retry that absorbs token propagation lag.
func (g *mcpGateway) toolWriter(ctx context.Context, target marktools.Target, _ string) (marktools.WriteFunc, error) {
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return nil, errors.New("internal: missing identity on tool-call context")
	}
	if err := g.writeRefusal(claims, target.Host); err != nil {
		return nil, err
	}
	world := target.Host
	return func(ctx context.Context, op func(token string) (fetch.Result, error)) (fetch.Result, error) {
		return g.dispatchWithWriteAuth(ctx, world, op)
	}, nil
}

// emptyGraphHint follows "No backlinks found".
const emptyGraphHint = "Run mark_graph to populate the broker's graph store. (Note: the broker's graph store is ephemeral; it resets on broker restart.)"

// toolGraph is the caller's graph scope, resolved once per call. Seed closes
// over that scope: a second resolution could land in a scope created after
// this one was retired, and fill it with this call's data.
func (g *mcpGateway) toolGraph(ctx context.Context) (*marktools.GraphScope, error) {
	state, err := g.graphFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph scope: %w", err)
	}
	return &marktools.GraphScope{
		Store: state.graphStore,
		Fetch: g.crawlFetchFn(ctx),
		Source: func(target marktools.Target) string {
			source, _ := g.worldSource(target.Host, target.Path) // unknown world: the fetch already failed
			return source
		},
		Seed:      func(ctx context.Context, _ marktools.Target) { g.seedGraphStore(ctx, state) },
		EmptyHint: emptyGraphHint,
	}, nil
}

// toolAgent is who the world's history shows as the writer: the verified email.
func toolAgent(ctx context.Context) string {
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return ""
	}
	return canonicalEmail(claims.Email)
}

// run answers a tool call with a shared tool body.
func (g *mcpGateway) run(call func(*marktools.Tools) marktools.Result) (*mcp.CallToolResult, error) {
	if g.tools == nil {
		return mcp.NewToolResultError("internal: tool bodies unavailable"), nil
	}
	result := call(g.tools)
	if result.IsError {
		return mcp.NewToolResultError(result.Text), nil
	}
	return mcp.NewToolResultText(result.Text), nil
}

// toolSiteErrorText names the cause an agent can act on: an unknown world or
// a refused identity; anything else is "<site> failed".
func toolSiteErrorText(site marktools.Site, worldName string, err error) string {
	var notFound *errWorldNotFound
	unknownWorld := errors.As(err, &notFound)
	switch {
	case site == marktools.SiteResolveCandidate && unknownWorld:
		// A candidate outside this system is a clean skip, not a broker error.
		return "not a knowledge-system world (broker has no config for this server)"
	case site == marktools.SiteResolveCandidate:
		return err.Error()
	case unknownWorld:
		return notFound.Error()
	case errors.Is(err, ErrNotAuthorized):
		return fmt.Sprintf("not authorized for world %q", worldName)
	case errors.Is(err, ErrEmailUnverified):
		return "identity email is not verified"
	}
	return fmt.Sprintf("%s failed: %v", site, err)
}
