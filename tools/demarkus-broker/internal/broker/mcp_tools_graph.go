package broker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/latebit/demarkus/client/fetch"
	"github.com/latebit/demarkus/client/graph"
	"github.com/latebit/demarkus/client/graphstore"
	"github.com/latebit/demarkus/client/index"
	"github.com/latebit/demarkus/client/links"
	"github.com/latebit/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// mcp_tools_graph.go ships the five graph-store-backed federation
// tools for Slice 4b+5 of the broker MCP gateway plan:
//
//   - mark_backlinks (4b) — local query against the broker's
//     ephemeral graph store. No dispatch.
//   - mark_graph (4b) — crawl world(s), persist to the broker's
//     ephemeral graph store, return the discovered graph.
//   - mark_index (5) — crawl a source world, collect content
//     hashes, build an index document, optionally publish to a
//     target world.
//   - mark_graph_export (5) — export the broker's ephemeral
//     graph store as a markdown document. Local-only.
//   - mark_graph_publish (5) — export + publish in one shot
//     against a target world.
//
// The broker's graph store is ephemeral per the plan v5 4b
// decision (Fritz, 2026-05-21): drops on every broker pod
// restart, agents re-crawl to repopulate. An alternate
// bucket-backed persistent store is parked for the post-broker
// design window (see /thoughts.md "On Bucket Stores as a
// k8s-Native Alternate Filestore").

// brokerCrawlParseURL splits a mark:// URL into worldName + path
// for the crawler. Lenient compared to parseToolURL — fragments
// and queries are stripped silently because links inside crawled
// documents legitimately carry them (e.g. `mark://team-a/foo.md#section`
// referring to a heading), and we want the crawler to fetch
// the document itself even when the link carries a fragment.
// parseToolURL stays strict for agent input, where a fragment
// is a typo signal.
func brokerCrawlParseURL(raw string) (worldName, urlPath string, err error) {
	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", "", parseErr
	}
	if u.Scheme != "mark" {
		return "", "", fmt.Errorf("not mark://: %s", u.Scheme)
	}
	worldName = u.Hostname()
	if worldName == "" {
		return "", "", fmt.Errorf("missing world name in %q", raw)
	}
	urlPath = u.Path
	if urlPath == "" {
		urlPath = "/"
	}
	return worldName, urlPath, nil
}

// crawlFetchFn returns a closure suitable for
// graphstore.CrawlAndPersist's fetchFunc parameter. Each per-URL
// fetch threads through dispatchWithAuth so the broker's
// session cache + propagation-race retry apply even inside the
// crawl loop. The closure intentionally tolerates per-fetch
// failure (returns the error to the crawler, which marks the
// node "error" and continues) — a graph crawl is best-effort.
func (g *mcpGateway) crawlFetchFn(ctx context.Context, claims *Claims) func(string, string) (string, string, string, error) {
	return func(worldName, path string) (string, string, string, error) {
		result, derr := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
			return g.dispatcher.Fetch(worldName, path, token)
		})
		if derr != nil {
			return "", "", "", derr
		}
		return result.Response.Status, result.Response.Body, result.Response.Metadata["etag"], nil
	}
}

// handleMarkBacklinks queries the broker's ephemeral graph store
// for documents linking to a given URL. Pure local read — no
// dispatch — because the graph store is the index of "what we've
// crawled" and that data already lives in the broker's process.
// First-time callers see an empty result (and a hint to run
// mark_graph first); broker-pod restarts reset to that state.
func (g *mcpGateway) handleMarkBacklinks(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	// Validate the URL shape so an agent typo doesn't silently
	// produce "no backlinks" against a malformed lookup key.
	if _, _, perr := parseToolURL(raw); perr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", perr)), nil
	}
	backlinks := g.graphStore.BacklinksEnriched(raw)
	if len(backlinks) == 0 {
		return mcp.NewToolResultText(
			fmt.Sprintf("No backlinks found for %s\nRun mark_graph to populate the broker's graph store. (Note: the broker's graph store is ephemeral; it resets on broker restart.)", raw),
		), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Backlinks for %s (%d):\n\n", raw, len(backlinks))
	for _, bl := range backlinks {
		if bl.Title != "" {
			fmt.Fprintf(&b, "- [%s](%s)\n", bl.Title, bl.URL)
		} else {
			fmt.Fprintf(&b, "- %s\n", bl.URL)
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

// handleMarkGraph crawls a seed URL and writes discovered nodes
// + edges to the broker's ephemeral graph store. Depth defaults
// to 2, capped at 5 (the local demarkus-mcp's convention). Each
// per-URL fetch inside the crawl goes through dispatchWithAuth
// so propagation-race retry + session-cache reuse apply.
//
// MaxNodes=200 matches the local server. Without a cap, a
// poorly-shaped graph (linking everything to everything) could
// pin the broker pod's memory; 200 is generous for the typical
// agent-knowledge-base shape.
func (g *mcpGateway) handleMarkGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	if _, _, perr := parseToolURL(raw); perr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", perr)), nil
	}
	depth := max(1, min(req.GetInt("depth", 2), 5))
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}

	crawled, crawlErr := g.graphStore.CrawlAndPersist(
		ctx,
		raw,
		g.crawlFetchFn(ctx, claims),
		brokerCrawlParseURL,
		graphstore.CrawlOptions{
			MaxDepth: depth,
			MaxNodes: 200,
			Workers:  5,
		},
	)
	if crawlErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("crawl failed: %v", crawlErr)), nil
	}
	return mcp.NewToolResultText(formatGraphSummary(crawled, raw)), nil
}

// formatGraphSummary renders a Graph as the same plain-text
// shape the local demarkus-mcp emits. Duplicated rather than
// hoisted because the function is ~25 LOC and stable; the
// proxy-fidelity philosophy (Slice 2's formatToolResult) applies
// equally here. If the local helper drifts, the proxy-fidelity
// test in mcp_tools_read_test.go will fail — we can hoist into
// a shared package then.
func formatGraphSummary(gr *graph.Graph, startURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Crawled %d nodes, %d edges from %s\n", gr.NodeCount(), gr.EdgeCount(), startURL)

	nodes := gr.AllNodes()
	if len(nodes) == 0 {
		return b.String()
	}

	b.WriteString("\nNodes:\n")
	for _, n := range nodes {
		title := n.Title
		if title == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&b, "  [%-9s] %-40s %q  %d links\n", n.Status, n.URL, title, n.LinkCount)
	}

	edges := gr.GetEdges()
	if len(edges) > 0 {
		b.WriteString("\nEdges:\n")
		for _, e := range edges {
			fmt.Fprintf(&b, "  %s -> %s\n", e.From, e.To)
		}
	}

	return b.String()
}

// handleMarkGraphExport returns the broker's ephemeral graph
// store as a publishable markdown document. Pure local read.
// Empty store → empty document (still valid markdown).
func (g *mcpGateway) handleMarkGraphExport(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	return mcp.NewToolResultText(g.graphStore.Export()), nil
}

// handleMarkGraphPublish exports the broker's ephemeral graph
// store and PUBLISHes it to a target world. Same expected_version
// + on_conflict semantics as mark_publish (Slice 3); on_conflict
// support is deliberately not part of this tool's surface today
// (the local demarkus-mcp doesn't expose it either — graph
// publishes are single-writer flows where conflict really means
// "the agent forgot to fetch first," not a merge case).
func (g *mcpGateway) handleMarkGraphPublish(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	worldName, urlPath, err := parseToolURL(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	expectedVersion, err := req.RequireInt("expected_version")
	if err != nil {
		return mcp.NewToolResultError("expected_version is required"), nil
	}
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be >= 0"), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}
	body := g.graphStore.Export()
	meta := agentMetaFromClaims(claims)
	result, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.Publish(worldName, urlPath, body, token, expectedVersion, meta)
	})
	if err != nil {
		return g.toolErrorFor("graph publish", worldName, err), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Published graph (%d nodes, %d edges) to %s\n", g.graphStore.NodeCount(), g.graphStore.EdgeCount(), raw)
	out.WriteString(formatToolResult(result, "version", "modified", "server-version"))
	return mcp.NewToolResultText(out.String()), nil
}

// maxIndexDocuments caps mark_index's crawl to keep the index
// document bounded — matches the local demarkus-mcp's limit. If
// a world has more than this, the index document is truncated
// with a warning. Operators wanting an exhaustive index for a
// huge world can run mark_index multiple times against
// disjoint subtrees and merge the results.
const maxIndexDocuments = 1000

// errIndexTruncated is sentinel-returned by walkDir to short-
// circuit the recursion when maxIndexDocuments is reached.
var errIndexTruncated = errors.New("document limit reached, index is truncated")

// handleMarkIndex crawls a source world, collects content
// hashes, and either returns the index document (dry_run) or
// publishes it to a target world. Mirrors the local
// demarkus-mcp's mark_index behavior, including the manifest
// safeguards: target world must have an agent manifest or
// force=true must be passed to override.
func (g *mcpGateway) handleMarkIndex(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	sourceURL, err := req.RequireString("source")
	if err != nil {
		return mcp.NewToolResultError("source is required"), nil
	}
	targetURL, err := req.RequireString("target")
	if err != nil {
		return mcp.NewToolResultError("target is required"), nil
	}
	sourceWorld, sourcePath, err := parseToolURL(sourceURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid source URL: %v", err)), nil
	}
	if sourcePath == "" {
		sourcePath = "/"
	}
	targetWorld, targetPath, err := parseToolURL(targetURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid target URL: %v", err)), nil
	}
	dryRun := req.GetBool("dry_run", false)
	force := req.GetBool("force", false)
	expectedVersion := req.GetInt("expected_version", 0)
	if expectedVersion < 0 {
		return mcp.NewToolResultError("expected_version must be non-negative"), nil
	}
	claims, ok := claimsFromCtx(ctx)
	if !ok {
		return mcp.NewToolResultError("internal: missing identity on tool-call context"), nil
	}

	// Manifest safeguards. Source warning is informational;
	// target absence is a hard block unless force or dry_run.
	warnings, block := g.checkIndexManifests(ctx, claims, sourceWorld, targetWorld, dryRun, force)
	if block != nil {
		return block, nil
	}

	// Crawl source world. The walkIndexDir helper handles depth
	// recursion + the maxIndexDocuments cap + cycle detection.
	var entries []index.Entry
	sourceScheme := "mark://" + sourceWorld
	// visited is the cycle-detection + path-escape gate. A world
	// that responds to LIST with directory entries linking back
	// to ancestor paths (whether by buggy server or hostile
	// content) would otherwise recurse forever — maxIndexDocuments
	// bounds file appends but not directory traversal. Keyed on
	// the canonical (path.Clean-normalized) directory path so two
	// representations of the same dir (e.g. /docs/ and /docs)
	// collapse to one entry.
	visited := map[string]struct{}{}
	// sourceRoot is the canonical form of the user-supplied
	// starting directory. walkIndexDir refuses to recurse into
	// paths that don't stay under it, blocking path-escape via
	// "../" in LIST destinations.
	sourceRoot := path.Clean(sourcePath)
	if !strings.HasSuffix(sourceRoot, "/") {
		sourceRoot += "/"
	}
	walkErr := g.walkIndexDir(ctx, claims, sourceWorld, sourcePath, sourceScheme, sourceRoot, &entries, visited)
	if walkErr != nil && !errors.Is(walkErr, errIndexTruncated) {
		return mcp.NewToolResultError(fmt.Sprintf("crawl failed: %v", walkErr)), nil
	}
	if errors.Is(walkErr, errIndexTruncated) {
		warnings = append(warnings, fmt.Sprintf("warning: index truncated at %d documents, some content may not be indexed", maxIndexDocuments))
	}

	body := index.Build(sourceScheme, time.Now(), entries)

	if dryRun {
		var b strings.Builder
		for _, w := range warnings {
			b.WriteString(w + "\n")
		}
		fmt.Fprintf(&b, "Indexed %d documents from %s (dry run, not published)\n\n", len(entries), sourceScheme)
		b.WriteString(body)
		return mcp.NewToolResultText(b.String()), nil
	}

	// Merge with existing index if updating an existing target.
	if expectedVersion > 0 {
		existing, fetchErr := g.dispatchWithAuth(ctx, claims, targetWorld, func(token string) (fetch.Result, error) {
			return g.dispatcher.Fetch(targetWorld, targetPath, token)
		})
		if fetchErr != nil {
			return g.toolErrorFor("index (fetch existing)", targetWorld, fetchErr), nil
		}
		if existing.Response.Status != protocol.StatusOK {
			return mcp.NewToolResultError(fmt.Sprintf("failed to fetch existing index: %s", existing.Response.Status)), nil
		}
		existingEntries := index.Parse(existing.Response.Body)
		merged := index.Merge(existingEntries, sourceScheme, entries)
		targetScheme := "mark://" + targetWorld
		body = index.Build(targetScheme, time.Now(), merged)
	}

	meta := agentMetaFromClaims(claims)
	result, err := g.dispatchWithAuth(ctx, claims, targetWorld, func(token string) (fetch.Result, error) {
		return g.dispatcher.Publish(targetWorld, targetPath, body, token, expectedVersion, meta)
	})
	if err != nil {
		return g.toolErrorFor("index (publish)", targetWorld, err), nil
	}

	var out strings.Builder
	for _, w := range warnings {
		out.WriteString(w + "\n")
	}
	fmt.Fprintf(&out, "Indexed %d documents from %s\n", len(entries), sourceScheme)
	out.WriteString(formatToolResult(result, "version", "modified"))
	return mcp.NewToolResultText(out.String()), nil
}

// checkIndexManifests applies the same source-warn / target-block
// semantics the local demarkus-mcp does: a source world without
// a manifest is a warning (the index may still be useful even if
// the source doesn't advertise itself), a target without a
// manifest is a block (the agent shouldn't write an index to a
// world that hasn't said "yes, accept index publications") —
// force=true bypasses the block with a recorded warning so an
// operator who intends the publish anyway can opt in.
func (g *mcpGateway) checkIndexManifests(ctx context.Context, claims *Claims, sourceWorld, targetWorld string, dryRun, force bool) ([]string, *mcp.CallToolResult) {
	var warnings []string
	srcManifest, err := g.dispatchWithAuth(ctx, claims, sourceWorld, func(token string) (fetch.Result, error) {
		return g.dispatcher.Fetch(sourceWorld, protocol.WellKnownManifestPath, token)
	})
	if err != nil || srcManifest.Response.Status != protocol.StatusOK {
		warnings = append(warnings, "warning: source server has no agent manifest")
	}
	if !dryRun {
		tgtManifest, terr := g.dispatchWithAuth(ctx, claims, targetWorld, func(token string) (fetch.Result, error) {
			return g.dispatcher.Fetch(targetWorld, protocol.WellKnownManifestPath, token)
		})
		if terr != nil || tgtManifest.Response.Status != protocol.StatusOK {
			if !force {
				return warnings, mcp.NewToolResultError(
					"target server has no agent manifest — cannot verify it accepts index publications. " +
						"Use force=true to override, or publish a manifest at /.well-known/agent-manifest.md on the target.",
				)
			}
			warnings = append(warnings, "warning: target server has no agent manifest (force=true override)")
		}
	}
	return warnings, nil
}

// walkIndexDir recursively LISTs a world's directory tree and
// FETCHes each file to collect its content-hash. Honors the
// maxIndexDocuments cap by returning errIndexTruncated, which
// the outer handler decodes into a "warning, index truncated"
// rather than a hard error. Inaccessible directories (a LIST
// that fails or returns non-ok) are skipped silently — they may
// be perfectly legitimate paths the broker's mint scope just
// doesn't cover this call.
//
// Two safety properties guard against pathological LIST
// responses (cycles, path escapes — whether from a misbehaving
// world or a malicious one):
//
//   - visited keys on canonical (path.Clean-normalized)
//     directory paths. A directory entry linking back to an
//     ancestor (or to itself) short-circuits before recursing.
//   - sourceRoot is the canonical form of the user-supplied
//     starting directory; directories whose canonical path
//     escapes this root (e.g. via "../" in a LIST destination)
//     are skipped silently. The index is "what's under
//     sourcePath", not "what the world feels like serving."
func (g *mcpGateway) walkIndexDir(ctx context.Context, claims *Claims, worldName, dirPath, sourceScheme, sourceRoot string, entries *[]index.Entry, visited map[string]struct{}) error {
	if len(*entries) >= maxIndexDocuments {
		return errIndexTruncated
	}
	listResult, err := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
		return g.dispatcher.List(worldName, dirPath, token)
	})
	if err != nil {
		return fmt.Errorf("list %s: %w", dirPath, err)
	}
	if listResult.Response.Status != protocol.StatusOK {
		// Skip silently — the world may be enforcing
		// path-level RBAC on this subtree. Other accessible
		// subtrees still index.
		return nil
	}
	for _, dest := range links.Extract(listResult.Response.Body) {
		if len(*entries) >= maxIndexDocuments {
			return errIndexTruncated
		}
		fullPath := dirPath
		if !strings.HasSuffix(fullPath, "/") {
			fullPath += "/"
		}
		fullPath += dest
		if strings.HasSuffix(dest, "/") {
			// Canonicalize before deciding whether to recurse.
			// path.Clean collapses ".", ".." and double-slashes
			// to a normalized form we can compare for cycles
			// and check against the source root.
			canonical := path.Clean(fullPath)
			if !strings.HasSuffix(canonical, "/") {
				canonical += "/"
			}
			if !strings.HasPrefix(canonical, sourceRoot) {
				// Path-escape attempt (e.g., a LIST entry
				// of "../" climbing above the source). Skip
				// silently — the index covers the subtree
				// the user pointed at, not the world's
				// whole filesystem.
				continue
			}
			if _, seen := visited[canonical]; seen {
				continue
			}
			visited[canonical] = struct{}{}
			if recErr := g.walkIndexDir(ctx, claims, worldName, fullPath, sourceScheme, sourceRoot, entries, visited); recErr != nil {
				return recErr
			}
			continue
		}
		// File — fetch and collect content-hash.
		doc, ferr := g.dispatchWithAuth(ctx, claims, worldName, func(token string) (fetch.Result, error) {
			return g.dispatcher.Fetch(worldName, fullPath, token)
		})
		if ferr != nil {
			continue
		}
		if doc.Response.Status != protocol.StatusOK {
			continue
		}
		contentHash := doc.Response.Metadata["content-hash"]
		if _, valid := protocol.IsHashPath(contentHash); !valid || contentHash == "" {
			continue
		}
		*entries = append(*entries, index.Entry{
			Hash:   contentHash,
			Server: sourceScheme,
			Path:   fullPath,
		})
	}
	return nil
}

// Compile-time guards: every handler conforms to mcp-go's
// ToolHandlerFunc shape. Same pattern as Slices 3 + 4a.
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkBacklinks
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkGraph
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkGraphExport
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkGraphPublish
var _ mcpserver.ToolHandlerFunc = (*mcpGateway)(nil).handleMarkIndex
