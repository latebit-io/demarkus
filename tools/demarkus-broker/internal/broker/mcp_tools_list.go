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
	"mark_list",
	"mark_versions",
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
}

// mcpTools returns the 13 tool definitions exposed by the broker MCP
// gateway. The set mirrors client/cmd/demarkus-mcp's tool surface for
// parity (so the agent sees the same vocabulary regardless of
// transport); only the URL-format description differs because the
// broker addresses worlds by name rather than host:port.
func mcpTools() []mcp.Tool {
	return []mcp.Tool{
		markFetchTool(),
		markListTool(),
		markVersionsTool(),
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
	}
}

func markFetchTool() mcp.Tool {
	return mcp.NewTool("mark_fetch",
		mcp.WithDescription(
			"Fetch a document from a Mark Protocol server. "+
				"Returns the document status, version, modified timestamp, etag, and markdown body. "+
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
				"Use this to discover what documents exist. "+
				mcpURLHint,
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(mcpURLDesc),
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
				"Returns results from previous crawls — run mark_graph first to populate. "+
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
				"or find broken links. When a graph store is available, results are "+
				"persisted for backlink queries. "+
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
	)
}
