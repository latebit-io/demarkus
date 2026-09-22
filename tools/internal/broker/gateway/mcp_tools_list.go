package gateway

import (
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
)

// mcpURLDesc is the per-argument description wherever a tool takes a
// mark:// URL. The broker addresses worlds by name, so there is no default
// host mode; the URL form is stated once in the server instructions.
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

func markFetchTool() mcp.Tool { return mcpfmt.FetchTool(mcpURLDesc, "") }

func markExploreTool() mcp.Tool { return mcpfmt.ExploreTool(mcpURLDesc, "") }

func markListTool() mcp.Tool { return mcpfmt.ListTool(mcpURLDesc, "") }

func markVersionsTool() mcp.Tool { return mcpfmt.VersionsTool(mcpURLDesc, "") }

func markLookupTool() mcp.Tool {
	options := append([]mcp.ToolOption{
		mcp.WithDescription(mcpfmt.LookupDescription + " A world without body match answers from the catalog and says so; system-wide: mark_lookup_all."),
		mcp.WithString("url", mcp.Required(), mcp.Description(mcpfmt.ScopeDesc("mark://{worldName}/"))),
	}, mcpfmt.LookupParams()...)
	return mcp.NewTool("mark_lookup", options...)
}

func markLookupAllTool() mcp.Tool {
	return mcp.NewTool("mark_lookup_all",
		mcp.WithDescription(
			"Catalog lookup by subject across all readable worlds. One globally limited table of mark://{worldName}/{path} rows; partial world failures reported with matches. match=body also matches section text; budget>0 appends sections.",
		),
		mcp.WithString("query", mcp.Required(), mcp.Description(mcpfmt.QueryDesc)),
		mcp.WithString("scope", mcp.Description("path applied to every world, e.g. / or /docs/ (default /)")),
		mcp.WithString("filter", mcp.Description(mcpfmt.FilterDesc)),
		mcp.WithNumber("limit", mcp.Description("global "+mcpfmt.LimitDesc)),
		mcp.WithString("match", mcp.Description(mcpfmt.MatchDesc+"; worlds without body match are listed")),
		lookupexpand.Option(),
		mcpfmt.LookupAll.Param(),
	)
}

func markPublishTool() mcp.Tool { return mcpfmt.PublishTool(mcpURLDesc, "") }

func markAppendTool() mcp.Tool { return mcpfmt.AppendTool(mcpURLDesc, "") }

func markArchiveTool() mcp.Tool { return mcpfmt.ArchiveTool(mcpURLDesc, "") }

func markDiscoverTool() mcp.Tool {
	return mcp.NewTool("mark_discover",
		mcp.WithDescription(mcpfmt.DiscoverDescription),
		mcp.WithString("url", mcp.Required(), mcp.Description(mcpURLDesc)),
	)
}

func markResolveTool() mcp.Tool {
	return mcp.NewTool("mark_resolve",
		mcp.WithDescription(mcpfmt.ResolveDescription),
		mcp.WithString("hash", mcp.Required(), mcp.Description(mcpfmt.HashDesc)),
		mcp.WithString("index", mcp.Required(), mcp.Description("hub hash index document, e.g. mark://hub/index.md")),
	)
}

func markIndexTool() mcp.Tool {
	return mcp.NewTool("mark_index",
		mcp.WithDescription(mcpfmt.IndexDescription),
		mcp.WithString("source", mcp.Required(), mcp.Description("world to crawl, e.g. mark://team-a/")),
		mcp.WithString("target", mcp.Required(), mcp.Description("index destination, e.g. mark://hub/index.md")),
		mcp.WithNumber("expected_version", mcp.Description(mcpfmt.IndexVersionDesc)),
		mcp.WithBoolean("dry_run", mcp.Description(mcpfmt.DryRunDesc)),
		mcp.WithBoolean("force", mcp.Description(mcpfmt.IndexForceDesc)),
	)
}

func markBacklinksTool() mcp.Tool { return mcpfmt.BacklinksTool(mcpURLDesc, "The store is per pod.") }

func markGraphTool() mcp.Tool { return mcpfmt.GraphTool(mcpURLDesc, "") }

func markGraphExportTool() mcp.Tool { return mcpfmt.GraphExportTool("") }

func markWorldsTool() mcp.Tool {
	return mcp.NewTool("mark_worlds",
		mcp.WithDescription("Readable worlds with a writable flag. Columns: world ({worldName} in mark://{worldName}/{path}), url, address, writable. Discover the system and pick a write destination."),
	)
}

func markGraphPublishTool() mcp.Tool {
	return mcpfmt.GraphPublishTool("graph document target, e.g. mark://hub/graph.md", "")
}
