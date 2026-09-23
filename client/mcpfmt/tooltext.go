package mcpfmt

import (
	"encoding/json"
	"fmt"

	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// Tool builders shared by demarkus-mcp and the broker gateway: a surface adds
// only its URL wording and an optional suffix, so one operation has one schema
// on both transports. Every string here is paid in tokens on each model turn.

// Descriptions the surfaces still compose themselves (lookup, discover,
// resolve, index) because their arguments differ by transport.
const (
	LookupDescription   = "Catalog lookup by subject: matches tags and title; match=body also matches section text. Importance-ranked table (path, importance, title, tags; body rows add #anchor and a snippet). budget>0 appends sections."
	DiscoverDescription = "Fetch the agent manifest (/.well-known/agent-manifest.md): purpose, key paths, auth, usage. not-found if absent."
	ResolveDescription  = "Resolve content by SHA-256 hash via a hub index document and fetch it."
	IndexDescription    = "Crawl a source, collect content hashes, publish a sharded hash index to target (typically a hub). Needs read access to source and publish access to the target manifest and .shards subtree."

	QueryDesc           = "subject matched against tags and titles (min 2 chars); '*' for all"
	FilterDesc          = "comma-separated key=value predicates: tag=, modified-after=, modified-before="
	LimitDesc           = "max results (default 10, cap 1000)"
	MatchDesc           = "catalog (default) or body"
	HashDesc            = "sha256-<64 lowercase hex>"
	IndexVersionDesc    = "existing index version at target; 0 to create"
	DryRunDesc          = "return index without publishing (default false)"
	IndexForceDesc      = "publish even without hub manifest at target (default false)"
	ExpectedVersionDesc = "version from prior fetch; 0 to create"
)

// ScopeDesc describes a lookup scope argument from the surface's example root.
func ScopeDesc(example string) string {
	return "scope: " + example + " or a subtree"
}

const (
	fetchDescription        = "Fetch a document: status, version, title, markdown body; over 8KB an outline (headings with #anchors), url#<anchor> one section, force=true the full body. Unchanged re-fetch returns a short notice."
	listDescription         = "List documents and subdirectories. Archived hidden unless include_archived; while complete=false pass next-cursor as cursor. For one document prefer mark_explore."
	versionsDescription     = "Version history: total and current version, hash chain validity, per-version timestamps."
	publishDescription      = "Publish or update a document (markdown body); expected_version from prior fetch, 0 to create; metadata replaces the current map, a note lists dropped tags or keys. On conflict the default on_conflict=merge returns a merged candidate body (git-style markers where both sides changed) to review and republish at publish-at-version."
	appendDescription       = "Append markdown to an existing document. expected_version optional; omitted or 0 resolves the current version. Catalog metadata (tags, importance, title, type) carries over; change it via mark_publish."
	archiveDescription      = "Archive a document: fetches as 'archived', history kept."
	backlinksDescription    = "Documents linking to a URL, from the revision-aware graph store, with bounded source revalidation. Freshness, provenance and typed relations also appear in mark_explore."
	graphDescription        = "Crawl outbound links from a document to depth: link graph with edge provenance and rel-* typed relations. External links recorded, not followed. Feeds the graph store for mark_backlinks."
	graphExportDescription  = "Export the graph store as publishable markdown with mark:// links. Run mark_graph first."
	graphPublishDescription = "Export the graph store and publish it as crawlable markdown (mark_graph_export + mark_publish). Run mark_graph first."

	forceDesc           = "full body regardless of size or unchanged status (default false)"
	includeArchivedDesc = "include archived documents and all-archived directories (default false)"
	cursorDesc          = "continuation cursor from the prior result"
	listPageSizeDesc    = "entries per page, 1-1000 (default 1000)"
	bodyDesc            = "markdown body"
	appendBodyDesc      = "markdown to append"
	appendVersionDesc   = "version from prior fetch; omit or 0 to auto-resolve"
	onConflictDesc      = "merge (default): merged candidate body to review and republish at publish-at-version; fail: raw conflict status"
	metadataDesc        = "string values stored with the document: tags (comma-separated) and importance (0-1) rank mark_lookup; rel-<predicate> declares a typed relation (e.g. rel-supersedes: /adr/0002.md); other keys stored opaquely. retention (positive int) permanently deletes all but the newest N versions on this and every later write carrying it: irreversible, confirm with the user first"
	depthDesc           = "max link depth (default 2, max 5)"
	graphRetentionDesc  = "graph versions to keep (default 20; 0 keeps all); older permanently pruned"
)

func describe(base, suffix string) mcp.ToolOption {
	if suffix != "" {
		base += " " + suffix
	}
	return mcp.WithDescription(base)
}

// FetchTool builds mark_fetch; urlDesc describes the surface's URL argument.
func FetchTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_fetch",
		describe(fetchDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc+"; #<anchor> for one section")),
		mcp.WithBoolean("force", mcp.Description(forceDesc)),
		Fetch.Param(),
	)
}

// ExploreTool builds mark_explore with the shared neighborhood arguments.
func ExploreTool(urlDesc, suffix string) mcp.Tool {
	neighborhoodParams := NeighborhoodParams()
	options := make([]mcp.ToolOption, 0, len(neighborhoodParams)+3)
	options = append(options,
		describe(ExploreDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
	options = append(options, neighborhoodParams...)
	options = append(options, Fetch.Param())
	return mcp.NewTool("mark_explore", options...)
}

// ListTool builds mark_list.
func ListTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_list",
		describe(listDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithBoolean("include_archived", mcp.Description(includeArchivedDesc)),
		mcp.WithString("cursor", mcp.Description(cursorDesc)),
		mcp.WithNumber("page_size",
			mcp.Description(listPageSizeDesc),
			mcp.Min(1), mcp.Max(protocol.MaxListPageSize), mcp.MultipleOf(1),
		),
	)
}

// VersionsTool builds mark_versions.
func VersionsTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_versions",
		describe(versionsDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
}

// GraphTool builds mark_graph.
func GraphTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_graph",
		describe(graphDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithNumber("depth", mcp.Description(depthDesc)),
	)
}

// BacklinksTool builds mark_backlinks.
func BacklinksTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_backlinks",
		describe(backlinksDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
}

// PublishTool builds mark_publish.
func PublishTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_publish",
		describe(publishDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithString("body", mcp.Required(), mcp.Description(bodyDesc)),
		mcp.WithNumber("expected_version", mcp.Required(), mcp.Description(ExpectedVersionDesc)),
		mcp.WithString("on_conflict", mcp.Description(onConflictDesc)),
		mcp.WithObject("metadata", mcp.Description(metadataDesc)),
	)
}

// AppendTool builds mark_append.
func AppendTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_append",
		describe(appendDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithString("body", mcp.Required(), mcp.Description(appendBodyDesc)),
		mcp.WithNumber("expected_version", mcp.Description(appendVersionDesc)),
	)
}

// ArchiveTool builds mark_archive.
func ArchiveTool(urlDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_archive",
		describe(archiveDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
}

// GraphExportTool builds mark_graph_export, which takes no arguments.
func GraphExportTool(suffix string) mcp.Tool {
	return mcp.NewTool("mark_graph_export", describe(graphExportDescription, suffix))
}

// GraphPublishTool builds mark_graph_publish; targetDesc describes the graph document argument.
func GraphPublishTool(targetDesc, suffix string) mcp.Tool {
	return mcp.NewTool("mark_graph_publish",
		describe(graphPublishDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(targetDesc)),
		mcp.WithNumber("expected_version", mcp.Required(), mcp.Description(ExpectedVersionDesc)),
		mcp.WithNumber("retention", mcp.Description(graphRetentionDesc)),
	)
}

// LookupParams are the lookup arguments both surfaces share after url.
func LookupParams() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("query", mcp.Required(), mcp.Description(QueryDesc)),
		mcp.WithString("filter", mcp.Description(FilterDesc)),
		mcp.WithNumber("limit", mcp.Description(LimitDesc)),
		mcp.WithString("match", mcp.Description(MatchDesc)),
		lookupexpand.Option(),
		Lookup.Param(),
	}
}

// Tool profiles. Lean omits the operator and federation tools that no plugin
// prompt references; full is the complete surface for direct consumers.
const (
	ProfileLean = "lean"
	ProfileFull = "full"
)

// AdvancedTools are absent from the lean profile. The plugin-prompts check
// fails when a prompt template references one of them. mark_archive stays
// in lean: /soul-archive is the only archive path a plugin user has.
var AdvancedTools = map[string]bool{
	"mark_discover":      true,
	"mark_resolve":       true,
	"mark_index":         true,
	"mark_graph_export":  true,
	"mark_graph_publish": true,
}

// ValidProfile rejects anything but the two named profiles. Surfaces call it
// at their input boundary (flag, config); the filters below trust the value.
func ValidProfile(profile string) error {
	if profile != ProfileLean && profile != ProfileFull {
		return fmt.Errorf("unknown tool profile %q (lean or full)", profile)
	}
	return nil
}

// ProfileIncludes reports whether the profile exposes the named tool.
func ProfileIncludes(profile, name string) bool {
	return profile != ProfileLean || !AdvancedTools[name]
}

// ProfileTools filters a full tool list down to the profile.
func ProfileTools(profile string, full []mcp.Tool) []mcp.Tool {
	selected := make([]mcp.Tool, 0, len(full))
	for i := range full {
		if ProfileIncludes(profile, full[i].Name) {
			selected = append(selected, full[i])
		}
	}
	return selected
}

// CheckSchemaBudget fails when the tools/list wire body exceeds budget bytes.
// Bytes track o200k tokens at about 4.1 per token, so surfaces can bound
// schema cost without a tokenizer.
func CheckSchemaBudget(tools []mcp.Tool, budget int) error {
	raw, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("marshal tool schemas: %w", err)
	}
	if len(raw) > budget {
		return fmt.Errorf("%d tool schemas = %d bytes, budget %d", len(tools), len(raw), budget)
	}
	return nil
}
