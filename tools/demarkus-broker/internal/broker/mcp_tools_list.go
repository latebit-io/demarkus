package broker

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// mcpURLHint is the URL-format suffix every gateway tool description
// carries. Unlike the local demarkus-mcp (which can be pinned to a
// single -host default), the broker addresses worlds by name — the
// worldName is the addressing primitive and the broker resolves it to
// a cluster-internal world server. There is no "default host" mode.
const mcpURLHint = "URL form: mark://{worldName}/{path}. {worldName} is one of the worlds in this knowledge system; the broker routes the request to the appropriate world server."

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
			"Fetch a document from a Mark Protocol server. "+
				"Returns the document status, version, modified timestamp, etag, and markdown body. "+
				"Documents under 8KB return the full body. Larger documents return an outline "+
				"instead: the heading tree with #anchors and per-section line counts plus the "+
				"opening paragraph, so you can pull just the section you need by appending "+
				"#<anchor> to the url (anchors are GitHub-style slugs; #section fetches work at "+
				"any size). Re-fetching a document whose full body was already returned this "+
				"session returns a short 'unchanged' notice when it has not changed. "+
				"Set force=true for the full body regardless of size or session history. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc+"; append #<anchor> to fetch a single section"),
		),
		mcp.WithBoolean("force",
			mcp.Description("return the full body even if the document is large or unchanged since an earlier fetch this session (default false)"),
		),
	)
}

func markExploreTool() mcp.Tool {
	return mcp.NewTool("mark_explore",
		mcp.WithDescription(
			"Orient around one document in a single call: its outline head (heading "+
				"tree with #anchors plus the opening paragraph), outbound links, recorded "+
				"backlinks, and sibling documents in the same directory; each section "+
				"capped at 10 entries. Use this instead of a fetch + backlinks + list "+
				"chain when you need to understand what a document covers and what to "+
				"read next; then mark_fetch url#<anchor> for the sections that matter. "+
				"Backlinks come from the broker's graph store (mark_graph populates it; "+
				"per-pod, resets on broker restart). "+
				mcpURLHint,
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
			"List documents and subdirectories on a Mark Protocol server. "+
				"Use this to discover what documents exist. To orient around one "+
				"specific document, prefer mark_explore: it bundles the sibling "+
				"listing with the outline, links, and backlinks. Archived documents are "+
				"hidden by default, along with directories that contain only archived "+
				"documents; set include_archived to true for a recovery/audit view. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithBoolean("include_archived",
			mcp.Description("Include archived documents (and all-archived directories) in the listing. Default false."),
		),
	)
}

func markVersionsTool() mcp.Tool {
	return mcp.NewTool("mark_versions",
		mcp.WithDescription(
			"Retrieve the version history of a document from a Mark Protocol server. "+
				"Returns total and current version numbers, hash chain validation status, "+
				"and a list of all versions with timestamps. "+
				mcpURLHint,
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
			"Look up documents by subject against a Mark Protocol server's catalog. "+
				"Matches the query against each document's declared tags and title and returns "+
				"an importance-ranked markdown table of matches (path, importance, title, tags), "+
				"not document bodies; FETCH the ones you want. This is a catalog lookup, not "+
				"full-text search: a subject that was never tagged or titled will not be found. "+
				"Optionally narrow with a comma-separated key=value filter and cap results with limit. "+
				"For a system-wide subject, prefer mark_lookup_all; use this tool when the target world is known. "+
				"Lookup finds candidate documents, "+
				"mark_explore the best match to orient, then mark_fetch url#<anchor> for the "+
				"sections you actually need; full bodies of large documents are rarely necessary. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("scope to search under: mark://{worldName}/ for the whole world, or a subtree like mark://{worldName}/docs/. "+mcpURLDesc),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("subject to look up; matched against document tags and titles (minimum 2 characters), or '*' to match every catalogued document under the scope (importance order)"),
		),
		mcp.WithString("filter",
			mcp.Description("comma-separated key=value predicates applied before ranking; built-ins: tag=, modified-after=, modified-before="),
		),
		mcp.WithNumber("limit",
			mcp.Description("maximum number of results (server default 10, hard cap 1000)"),
		),
	)
}

func markLookupAllTool() mcp.Tool {
	return mcp.NewTool("mark_lookup_all",
		mcp.WithDescription(
			"Look up documents by subject across every world your identity may read. "+
				"Returns one globally limited markdown table whose paths are qualified as "+
				"mark://{worldName}/{path}. Results retain each world's relevance rank and "+
				"use importance and world identity for deterministic ordering. Partial world "+
				"failures are reported alongside successful matches. This is a broker-wide "+
				"catalog lookup, not full-text search.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("subject to look up; matched against document tags and titles (minimum 2 characters), or '*' to match every catalogued document"),
		),
		mcp.WithString("scope",
			mcp.Description("server-relative scope applied to every world, e.g. / or /docs/ (default /)"),
		),
		mcp.WithString("filter",
			mcp.Description("comma-separated key=value predicates applied in every world; built-ins: tag=, modified-after=, modified-before="),
		),
		mcp.WithNumber("limit",
			mcp.Description("global maximum number of results across all worlds (default 10, hard cap 1000)"),
		),
	)
}

func markPublishTool() mcp.Tool {
	return mcp.NewTool("mark_publish",
		mcp.WithDescription(
			"Publish or update a document on a Mark Protocol server. "+
				"Returns the created version number and modified timestamp. "+
				"The body should be valid markdown content. "+
				"expected_version is required for optimistic concurrency: set it to the version "+
				"number from a prior fetch to detect conflicts. If the document has been "+
				"modified since that version, the server returns a conflict status. "+
				"Use 0 when creating a new document. "+
				"on_conflict controls behavior when the version check fails. Default \"merge\" "+
				"returns a structurally-merged candidate body (with git-style conflict markers "+
				"if both sides edited the same lines) for the agent to semantically verify, "+
				"then call mark_publish again with expected_version set to the returned "+
				"publish-at-version. This prevents the silent content loss that happens when "+
				"callers naively republish their stale body. Pass \"fail\" to opt out and get "+
				"the strict conflict response with no merge attempt. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown content to publish"),
		),
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version number from a prior fetch for conflict detection; use 0 when creating a new document"),
		),
		mcp.WithString("on_conflict",
			mcp.Description("conflict behavior: \"merge\" (default) returns a merge-candidate body; the agent reviews it (resolving any conflict markers) and calls mark_publish again with expected_version set to the returned publish-at-version. \"fail\" opts out and returns the raw conflict status."),
		),
		mcp.WithObject("metadata",
			mcp.Description("optional publisher metadata stored with the document, as string values. The server interprets `tags` (comma-separated subject labels) and `importance` (0-1) for mark_lookup ranking; other keys are stored opaquely. Keys with the `rel-` prefix declare typed relations to other documents (e.g. `rel-supersedes: /adr/0002.md`, comma-separated for multiple targets); graph crawls ingest them as typed edges. Reserved keys are rejected. `retention` (positive integer) caps version history: this write and every later write carrying the key permanently delete versions older than the newest N. Destructive and irreversible: confirm with the user before setting it on a document whose history matters; intended for generated documents."),
		),
	)
}

func markAppendTool() mcp.Tool {
	return mcp.NewTool("mark_append",
		mcp.WithDescription(
			"Append content to the end of an existing document on a Mark Protocol server. "+
				"The server concatenates the new content after the existing document body. "+
				"Returns the created version number and modified timestamp. "+
				"The body should be valid markdown content to append. "+
				"expected_version is optional: when omitted or 0, the tool calls VERSIONS to get the "+
				"current version automatically. Set it explicitly if you already know the version "+
				"from a prior fetch. "+
				"The document keeps its catalog metadata across an append: the server carries the "+
				"current version's tags, importance, title, and type onto the new version, so an "+
				"append never drops a document out of mark_lookup or its policy tag axes. Use "+
				"mark_publish to change or remove metadata. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithString("body",
			mcp.Required(),
			mcp.Description("markdown content to append"),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("version number from a prior fetch for conflict detection; when omitted or 0, resolved via VERSIONS"),
		),
	)
}

func markArchiveTool() mcp.Tool {
	return mcp.NewTool("mark_archive",
		mcp.WithDescription(
			"Archive a document on a Mark Protocol server. "+
				"Returns the archived version number. "+
				"Archived documents return 'archived' status on FETCH but version history is preserved. "+
				mcpURLHint,
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
			"Fetch the agent manifest from a Mark Protocol server. "+
				"Returns the manifest at /.well-known/agent-manifest.md which describes "+
				"the server's purpose, key paths, auth requirements, and usage guidelines. "+
				"Returns not-found if no manifest is published. "+
				mcpURLHint,
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
			"Resolve content by its SHA-256 hash using a hub index document. "+
				"Looks up the hash in the index, finds servers hosting the content, "+
				"and fetches it by hash. Returns the document content if found. "+
				mcpURLHint,
		),
		mcp.WithString("hash",
			mcp.Required(),
			mcp.Description("content hash to resolve (sha256-<64 lowercase hex characters>)"),
		),
		mcp.WithString("index",
			mcp.Required(),
			mcp.Description("mark:// URL of the hash index document on a hub world, e.g. mark://hub/index.md"),
		),
	)
}

func markIndexTool() mcp.Tool {
	return mcp.NewTool("mark_index",
		mcp.WithDescription(
			"Crawl a Mark Protocol server, collect content hashes from all documents, "+
				"and publish a hash index document to a target server (typically a hub). "+
				"The tool checks the target's agent manifest before publishing. "+
				mcpURLHint,
		),
		mcp.WithString("source",
			mcp.Required(),
			mcp.Description("source world URL to crawl, e.g. mark://team-a/"),
		),
		mcp.WithString("target",
			mcp.Required(),
			mcp.Description("target world URL where the index should be published, e.g. mark://hub/index.md"),
		),
		mcp.WithNumber("expected_version",
			mcp.Description("version of existing index at target for conflict detection; use 0 to create new"),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("if true, return the index document without publishing (default false)"),
		),
		mcp.WithBoolean("force",
			mcp.Description("if true, publish even when target has no hub manifest (default false)"),
		),
	)
}

func markBacklinksTool() mcp.Tool {
	return mcp.NewTool("mark_backlinks",
		mcp.WithDescription(
			"Look up which documents link to a given URL, using the broker's graph store. "+
				"Returns results from previous crawls; run mark_graph first to populate. "+
				"The store is seeded from the world's published /graph.md when available, "+
				"so backlinks answer even on a cold pod; local crawls take precedence. "+
				"Each backlink shows its provenance (link label, source section anchor, "+
				"occurrence count) and typed relations like [supersedes] when present. "+
				"NOTE: the broker's graph store is ephemeral (per-pod lifetime); a broker restart "+
				"resets crawled data and re-seeds from /graph.md on the next call. "+
				"mark_explore includes this same backlink list alongside the document's "+
				"outline, outbound links, and siblings; prefer it for general orientation. "+
				mcpURLHint,
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
			"Crawl outbound links from a document and return the link graph. "+
				"Follows mark:// links up to the specified depth. External links are "+
				"recorded but not followed. Use this to understand document relationships "+
				"or find broken links. Edges carry provenance (link label, source section "+
				"anchor, occurrence count) and typed relations ingested from rel-<predicate> "+
				"publisher metadata. When a graph store is available, results are "+
				"persisted for backlink queries; the store is seeded from the world's "+
				"published /graph.md when available, and local crawls take precedence. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
		),
		mcp.WithNumber("depth",
			mcp.Description("Maximum link depth to follow (default 2, max 5)"),
		),
	)
}

func markGraphExportTool() mcp.Tool {
	return mcp.NewTool("mark_graph_export",
		mcp.WithDescription(
			"Export the broker's graph store as a publishable markdown document. "+
				"The output contains mark:// links so crawling it naturally discovers the topology. "+
				"Run mark_graph first to populate the store.",
		),
	)
}

func markWorldsTool() mcp.Tool {
	return mcp.NewTool("mark_worlds",
		mcp.WithDescription(
			"List the worlds of this knowledge system that your identity may read, "+
				"each flagged with whether you may also write it. "+
				"Returns a count and a markdown table with columns: world (the "+
				"{worldName} addressing primitive for every other tool: "+
				"mark://{worldName}/{path}), url (the world's external address for a "+
				"direct client, may be blank), address (the world's internal dial "+
				"address, its identity in the topology graph), and writable (yes/no, "+
				"whether your identity may publish to the world; reading a world does "+
				"not imply you can write it). Use this to discover the universe before "+
				"navigating it, and to pick a write destination among the worlds you "+
				"may publish to.",
		),
	)
}

func markGraphPublishTool() mcp.Tool {
	return mcp.NewTool("mark_graph_publish",
		mcp.WithDescription(
			"Export the broker's graph store and publish it to a Mark Protocol server. "+
				"Combines mark_graph_export + mark_publish in one step. "+
				"The published document contains mark:// links so other agents can crawl it "+
				"to discover the topology without recrawling the original servers. "+
				"Run mark_graph first to populate the store. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("target URL where the graph document should be published, e.g. mark://hub/graph.md"),
		),
		mcp.WithNumber("expected_version",
			mcp.Required(),
			mcp.Description("version number from a prior fetch for conflict detection; use 0 when creating a new document"),
		),
		mcp.WithNumber("retention",
			mcp.Description("how many versions of the graph document to keep (default 20). The graph is regenerated on every publish, so older versions are permanently pruned. Pass 0 to keep every version."),
		),
	)
}
