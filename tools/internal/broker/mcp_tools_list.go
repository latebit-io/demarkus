package broker

import (
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// mcpURLHint is the URL-format suffix every gateway tool description
// carries. Unlike the local demarkus-mcp (which can be pinned to a
// single -host default), the broker addresses worlds by name — the
// worldName is the addressing primitive and the broker resolves it to
// a cluster-internal world server. There is no "default host" mode.
const mcpURLHint = "URLs: mark://{worldName}/{path}; worlds via mark_worlds."

// mcpURLDesc is the per-argument description used wherever a tool takes
// a mark:// URL. Kept short because the framework wires it into the
// JSON schema field-by-field; the longer guidance lives in mcpURLHint
// on each tool's description.
const mcpURLDesc = "mark:// URL, e.g. mark://team-a/index.md"

// mcpToolNames is the canonical list of tool names the broker MCP
// gateway exposes. The order matches mcpTools() — both lists are
// kept in sync so a tools/list regression test can assert the
// expected surface in one fixed comparison rather than parsing every
// description.
var mcpToolNames = []string{
	"mark_fetch",
	"mark_explore",
	"mark_list",
	"mark_versions",
	"mark_lookup",
	"mark_lookup_all",
	"mark_publish",
	"mark_append",
	"mark_archive",
	"mark_discover",
	"mark_resolve",
	"mark_index",
	"mark_backlinks",
	"mark_graph",
	"mark_graph_export",
	"mark_graph_publish",
	"mark_worlds",
}

// mcpTools returns 15 direct-MCP parity tools plus broker-only mark_worlds and
// mark_lookup_all, which operate on the knowledge system rather than one world.
func mcpTools() []mcp.Tool {
	return []mcp.Tool{
		markFetchTool(),
		markExploreTool(),
		markListTool(),
		markVersionsTool(),
		markLookupTool(),
		markLookupAllTool(),
		markPublishTool(),
		markAppendTool(),
		markArchiveTool(),
		markDiscoverTool(),
		markResolveTool(),
		markIndexTool(),
		markBacklinksTool(),
		markGraphTool(),
		markGraphExportTool(),
		markGraphPublishTool(),
		markWorldsTool(),
	}
}

func markFetchTool() mcp.Tool {
	return mcp.NewTool("mark_fetch",
		mcp.WithDescription(
			"Fetch a document: status, version, etag, markdown body. Over 8KB returns outline (headings with #anchors); url#<anchor> fetches one section, force=true the full body. Unchanged re-fetch returns short notice. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc+"; append #<anchor> to fetch a single section"),
		),
		mcp.WithBoolean("force",
			mcp.Description("full body regardless of size or unchanged status (default false)"),
		),
	)
}

func markExploreTool() mcp.Tool {
	return mcp.NewTool("mark_explore",
		mcp.WithDescription(
			"Orient around one document: outline head, outbound links, backlinks, siblings (10 each). Use instead of fetch+backlinks+list, then mark_fetch url#<anchor>. Backlinks from broker graph store (mark_graph populates; per-pod, resets on restart). "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
	)
}

func markListTool() mcp.Tool {
	return mcp.NewTool("mark_list",
		mcp.WithDescription(
			"List documents and subdirectories. Archived hidden unless include_archived. If complete=false, pass next-cursor as cursor. For one document prefer mark_explore. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithBoolean("include_archived",
			mcp.Description("include archived documents and all-archived directories (default false)"),
		),
		mcp.WithString("cursor",
			mcp.Description("continuation cursor from prior mark_list result"),
		),
		mcp.WithNumber("page_size",
			mcp.Description("entries per page, 1-1000 (default 1000)"),
			mcp.Min(1), mcp.Max(protocol.MaxListPageSize), mcp.MultipleOf(1),
		),
	)
}

func markVersionsTool() mcp.Tool {
	return mcp.NewTool("mark_versions",
		mcp.WithDescription(
			"Document version history: total and current version, hash chain validity, per-version timestamps. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
	)
}

func markLookupTool() mcp.Tool {
	return mcp.NewTool("mark_lookup",
		mcp.WithDescription(
			"Catalog lookup by subject in one world: matches tags and title only (not full text); returns importance-ranked table (path, importance, title, tags), no bodies. System-wide: mark_lookup_all. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("scope: mark://{worldName}/ or subtree like mark://{worldName}/docs/"),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("subject matched against tags and titles (min 2 chars); '*' for all catalogued"),
		),
		mcp.WithString("filter",
			mcp.Description("comma-separated key=value predicates; built-ins: tag=, modified-after=, modified-before="),
		),
		mcp.WithNumber("limit",
			mcp.Description("max results (default 10, cap 1000)"),
		),
	)
}

func markLookupAllTool() mcp.Tool {
	return mcp.NewTool("mark_lookup_all",
		mcp.WithDescription(
			"Catalog lookup by subject across all readable worlds. One globally limited table of mark://{worldName}/{path} rows; partial world failures reported with matches. Not full-text search. "+mcpURLHint,
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("subject matched against tags and titles (min 2 chars); '*' for all catalogued"),
		),
		mcp.WithString("scope",
			mcp.Description("scope applied to every world, e.g. / or /docs/ (default /)"),
		),
		mcp.WithString("filter",
			mcp.Description("comma-separated key=value predicates; built-ins: tag=, modified-after=, modified-before="),
		),
		mcp.WithNumber("limit",
			mcp.Description("global max results across worlds (default 10, cap 1000)"),
		),
	)
}

func markPublishTool() mcp.Tool {
	return mcp.NewTool("mark_publish",
		mcp.WithDescription(
			"Publish or update a document (markdown body). expected_version: version from prior fetch, 0 to create. On conflict, default on_conflict=merge returns merged candidate body (git-style markers where both sides changed): review, republish at returned publish-at-version. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown body"),
		),
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version from prior fetch; 0 to create"),
		),
		mcp.WithString("on_conflict",
			mcp.Description("\"merge\" (default): merge-candidate body to review and republish at returned publish-at-version; \"fail\": raw conflict status"),
		),
		mcp.WithObject("metadata",
			mcp.Description("string values stored with document. tags (comma-separated) and importance (0-1) drive mark_lookup ranking; rel-<predicate> keys declare typed relations (e.g. rel-supersedes: /adr/0002.md); other keys stored opaquely. retention (positive int) permanently deletes all but newest N versions on this and every later write carrying it: irreversible, confirm with user first"),
		),
	)
}

func markAppendTool() mcp.Tool {
	return mcp.NewTool("mark_append",
		mcp.WithDescription(
			"Append markdown to an existing document. expected_version optional; omitted or 0 resolves current version. Catalog metadata (tags, importance, title, type) carries over; change via mark_publish. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown to append"),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("version from prior fetch; omit or 0 to auto-resolve"),
		),
	)
}

func markArchiveTool() mcp.Tool {
	return mcp.NewTool("mark_archive",
		mcp.WithDescription(
			"Archive a document: fetches as 'archived', history kept. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
	)
}

func markDiscoverTool() mcp.Tool {
	return mcp.NewTool("mark_discover",
		mcp.WithDescription(
			"Fetch agent manifest (/.well-known/agent-manifest.md): purpose, key paths, auth, usage. not-found if absent. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
	)
}

func markResolveTool() mcp.Tool {
	return mcp.NewTool("mark_resolve",
		mcp.WithDescription(
			"Resolve content by SHA-256 hash via hub index document; fetch it. "+mcpURLHint,
		),
		mcp.WithString("hash",
			mcp.Required(),
			mcp.Description("sha256-<64 lowercase hex>"),
		),
		mcp.WithString("index",
			mcp.Required(),
			mcp.Description("hub hash index document, e.g. mark://hub/index.md"),
		),
	)
}

func markIndexTool() mcp.Tool {
	return mcp.NewTool("mark_index",
		mcp.WithDescription(
			"Crawl a world, collect content hashes, publish sharded hash index to target world (typically a hub). Target manifest and .shards must be publicly readable; world token needs publish access to both. "+mcpURLHint,
		),
		mcp.WithString("source",
			mcp.Required(),
			mcp.Description("world to crawl, e.g. mark://team-a/"),
		),
		mcp.WithString("target",
			mcp.Required(),
			mcp.Description("index destination, e.g. mark://hub/index.md"),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("existing index version at target; 0 to create"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("return index without publishing (default false)"),
		),
		mcp.WithBoolean("force",
			mcp.Description("publish even without hub manifest at target (default false)"),
		),
	)
}

func markBacklinksTool() mcp.Tool {
	return mcp.NewTool("mark_backlinks",
		mcp.WithDescription(
			"Documents linking to a URL, from broker graph store (seeded from world /graph.md; mark_graph crawls take precedence; per-pod, resets on restart). Entries show provenance and typed relations. mark_explore includes same list. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
	)
}

func markGraphTool() mcp.Tool {
	return mcp.NewTool("mark_graph",
		mcp.WithDescription(
			"Crawl outbound links from a document to depth; returns link graph with edge provenance and rel-* typed relations. External links recorded, not followed. Feeds graph store for mark_backlinks. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithNumber("depth",
			mcp.Description("max link depth (default 2, max 5)"),
		),
	)
}

func markGraphExportTool() mcp.Tool {
	return mcp.NewTool("mark_graph_export",
		mcp.WithDescription(
			"Export broker graph store as publishable markdown with mark:// links. Run mark_graph first.",
		),
	)
}

func markWorldsTool() mcp.Tool {
	return mcp.NewTool("mark_worlds",
		mcp.WithDescription(
			"Readable worlds, flagged writable yes/no. Columns: world ({worldName} in mark://{worldName}/{path}), url, address, writable. Use to discover the system and pick a write destination.",
		),
	)
}

func markGraphPublishTool() mcp.Tool {
	return mcp.NewTool("mark_graph_publish",
		mcp.WithDescription(
			"Export broker graph store and publish as crawlable markdown (mark_graph_export + mark_publish). Run mark_graph first. "+mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("graph document target, e.g. mark://hub/graph.md"),
		),
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version from prior fetch; 0 to create"),
		),
		mcp.WithNumber("retention",
			mcp.Description("graph versions to keep (default 20; 0 keeps all); older permanently pruned"),
		),
	)
}
