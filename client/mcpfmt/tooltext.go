package mcpfmt

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/memory/lookupexpand"
	"github.com/latebit-io/demarkus/client/memory/promote"
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
	QueryDesc           = "subject matched against tags and titles (min 2 chars); '*' for all"
	FilterDesc          = "comma-separated key=value predicates: tag=, modified-after=, modified-before="
	LimitDesc           = "max results (default 10, cap 1000)"
	MatchDesc           = "catalog (default) or body"
	ExpectedVersionDesc = "version from prior fetch; 0 to create"
)

// ScopeDesc describes a lookup scope argument from the surface's example root.
func ScopeDesc(example string) string {
	return "scope: " + example + " or a subtree"
}

const (
	fetchDescription        = "Fetch a document: status, version, title, markdown body; over 8KB an outline (headings with #anchors), url#<anchor> one section, force=true the full body. Unchanged re-fetch returns a short notice. <dir>/.context reads a session primer: hub, newest journal entries, active work."
	listDescription         = "List documents and subdirectories. Archived hidden unless include_archived; while complete=false pass next-cursor as cursor. For one document prefer mark_explore."
	versionsDescription     = "Version history: total and current version, hash chain validity, per-version timestamps."
	publishDescription      = "Publish or update a document (markdown body); expected_version from prior fetch, 0 to create; metadata replaces the current map; a first version is linked from its hub (hubs:); a note lists dropped tags or keys, style problems, a hub not linked. On conflict the default on_conflict=merge returns a merged candidate body (git-style markers where both sides changed) to review and republish at publish-at-version."
	appendDescription       = "Append markdown to an existing document. expected_version optional; omitted or 0 resolves the current version. Catalog metadata (tags, importance, title, type) carries over; change it via mark_publish. route=today: url is a directory, the entry goes to its journal/<YYYY-MM-DD>.md (created when absent, hubs linked, tags widened; an archived scope refuses); a repeated entry is reported present, not written."
	archiveDescription      = "Archive a document: fetches as 'archived', history kept. recursive=true: url is a directory, every live document under it. Ancestor hubs drop the bullets that only linked what was archived."
	curateDescription       = "Bring one document into shape: style findings, a proposed mechanical fix (an edit, or at 8KB a split into a hub plus topic files with anchors kept), inbound links to repoint, a proposal hash. A directory url (trailing /) proposes mark_doctor's fixable findings under it, up to 100 writes. apply=true with the hash writes it at the reported versions."
	promoteDescription      = "Promote a document to a shared knowledge system, by phase. A write needs the packet from the report before it."
	backlinksDescription    = "Documents linking to a URL, from the revision-aware graph store, with bounded source revalidation. Freshness, provenance and typed relations also appear in mark_explore."
	graphDescription        = "Crawl outbound links from a document to depth: link graph with edge provenance and rel-* typed relations. External links recorded, not followed. Feeds the graph store for mark_backlinks."
	graphExportDescription  = "Export the graph store as publishable markdown with mark:// links. Run mark_graph first."
	graphPublishDescription = "Export the graph store and publish it as crawlable markdown (mark_graph_export + mark_publish). Run mark_graph first."
	doctorDescription       = "Read-only hygiene audit of a store or directory: broken links, orphans, missing hubs, untagged or untyped documents, write-policy compliance under the floor (axes, fields, allowed values), secrets and personal data, duplicates, hub and document shape, style, a stale style guide, ADR gaps. Bounded; Coverage lists gaps. deep: metadata lost across versions. promotion: candidates for a shared knowledge system."

	forceDesc           = "full body regardless of size or unchanged status (default false)"
	includeArchivedDesc = "include archived documents and all-archived directories (default false)"
	cursorDesc          = "continuation cursor from the prior result"
	listPageSizeDesc    = "entries per page, 1-1000 (default 1000)"
	bodyDesc            = "markdown body"
	appendBodyDesc      = "markdown to append"
	appendVersionDesc   = "version from prior fetch; omit or 0 to auto-resolve"
	appendRouteDesc     = "today: url names a directory and the entry lands in its dated journal"
	appendTagsDesc      = "with route=today: comma-separated subjects of the entry, added to the journal's tags"
	onConflictDesc      = "merge (default): merged candidate body to review and republish at publish-at-version; fail: raw conflict status"
	metadataDesc        = "string values stored with the document: tags (comma-separated) and importance (0-1) rank mark_lookup; rel-<predicate> declares a typed relation (e.g. rel-supersedes: /adr/0002.md); other keys stored opaquely. retention (positive int) permanently deletes all but the newest N versions on this and every later write carrying it: irreversible, confirm with the user first"
	depthDesc           = "max link depth (default 2, max 5)"
	recursiveDesc       = "archive every live document under the directory url (default false)"
	curateApplyDesc     = "write the proposal (default false: report only)"
	curateProposalDesc  = "the proposal hash from the report; required with apply"
	promotePhaseDesc    = "which phase to run"
	doctorDeepDesc      = "walk version history for lost metadata (default false)"
	doctorPromotionDesc = "list promotion candidates, promoted ones excluded (default false)"
	graphRetentionDesc  = "graph versions to keep (default 20; 0 keeps all); older permanently pruned"
	budgetDesc          = "result tokens for the whole result: matched sections follow the table in rank order, then related documents of the strongest match, under '" + lookupexpand.Delimiter + " path/vN#anchor' lines, whole sections, until spent (default 0: table only)"
)

func describe(base, suffix string) mcp.ToolOption {
	if suffix != "" {
		base += " " + suffix
	}
	return mcp.WithDescription(base)
}

// NewTool builds a classified mark_* tool: reads get the read-only hints,
// since mcp-go defaults every tool to destructive. An unclassified name panics.
func NewTool(name string, opts ...mcp.ToolOption) mcp.Tool {
	class, ok := toolClasses[name]
	if !ok {
		panic("mcpfmt: unclassified tool " + name + ": add it to toolClasses")
	}
	if !class.write {
		opts = append(slices.Clip(opts), mcp.WithReadOnlyHintAnnotation(true), mcp.WithDestructiveHintAnnotation(false))
	}
	return mcp.NewTool(name, opts...)
}

// FetchTool builds mark_fetch; urlDesc describes the surface's URL argument.
func FetchTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_fetch",
		describe(fetchDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc+"; #<anchor> for one section")),
		mcp.WithBoolean("force", mcp.Description(forceDesc)),
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
	return NewTool("mark_explore", options...)
}

// ListTool builds mark_list.
func ListTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_list",
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
	return NewTool("mark_versions",
		describe(versionsDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
}

// GraphTool builds mark_graph.
func GraphTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_graph",
		describe(graphDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithNumber("depth", mcp.Description(depthDesc)),
	)
}

// BacklinksTool builds mark_backlinks.
func BacklinksTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_backlinks",
		describe(backlinksDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
	)
}

// PublishTool builds mark_publish.
func PublishTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_publish",
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
	return NewTool("mark_append",
		describe(appendDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithString("body", mcp.Required(), mcp.Description(appendBodyDesc)),
		mcp.WithNumber("expected_version", mcp.Description(appendVersionDesc)),
		mcp.WithString("route", mcp.Description(appendRouteDesc)),
		mcp.WithString("tags", mcp.Description(appendTagsDesc)),
	)
}

// ArchiveTool builds mark_archive.
func ArchiveTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_archive",
		describe(archiveDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithBoolean("recursive", mcp.Description(recursiveDesc)),
	)
}

// CurateTool builds mark_curate.
func CurateTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_curate",
		describe(curateDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithBoolean("apply", mcp.Description(curateApplyDesc)),
		mcp.WithString("proposal", mcp.Description(curateProposalDesc)),
	)
}

// PromoteTool builds mark_promote for the phases a surface serves; an
// argument is advertised, and its phases named, only where a served phase reads it.
func PromoteTool(urlDesc, suffix string, phases []string) mcp.Tool {
	options := []mcp.ToolOption{
		describe(promoteDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithString("phase", mcp.Required(), mcp.Enum(phases...), mcp.Description(promotePhaseDesc)),
	}
	for _, arg := range promoteArgs {
		if desc := arg.phaseDesc(phases); desc != "" {
			options = append(options, arg.with(arg.name, mcp.Description(desc)))
		}
	}
	return NewTool("mark_promote", options...)
}

// phaseClause is what an argument carries for the phases that read it.
type phaseClause struct {
	phases []string
	text   string
}

type promoteArg struct {
	name    string
	with    func(string, ...mcp.PropertyOption) mcp.ToolOption
	clauses []phaseClause
}

var promoteArgs = []promoteArg{
	{"body", mcp.WithString, []phaseClause{
		{[]string{promote.PhaseDryRun, promote.PhaseApply}, "the distilled draft"},
		{[]string{promote.PhaseRefresh}, "the live knowledge body"},
	}},
	{"metadata", mcp.WithObject, []phaseClause{
		{[]string{promote.PhaseDryRun, promote.PhaseApply}, "the draft's metadata object (tags, importance, type, the policy's axes)"},
	}},
	{"source", mcp.WithString, []phaseClause{
		{[]string{promote.PhaseDryRun, promote.PhaseApply}, "origin recorded as promoted-from (default none)"},
	}},
	{"target", mcp.WithString, []phaseClause{
		{[]string{promote.PhaseStamp, promote.PhaseRefresh}, "the knowledge copy as <url>@v<N>"},
	}},
	{"via", mcp.WithString, []phaseClause{
		{[]string{promote.PhaseStamp}, "the knowledge server's name, recorded as promoted-via"},
	}},
	{"stub", mcp.WithBoolean, []phaseClause{
		{[]string{promote.PhaseStamp}, "reduce the body to its title and summary (default false: keep it)"},
	}},
	{"packet", mcp.WithString, []phaseClause{
		{[]string{promote.PhaseApply, promote.PhaseStamp, promote.PhaseRefresh}, "the packet hash from the report before it"},
	}},
}

// phaseDesc renders the clauses a served phase reads, naming only served
// phases; "" means no served phase reads the argument.
func (a promoteArg) phaseDesc(served []string) string {
	var parts []string
	for _, c := range a.clauses {
		var names []string
		for _, phase := range c.phases {
			if slices.Contains(served, phase) {
				names = append(names, phase)
			}
		}
		if len(names) > 0 {
			parts = append(parts, strings.Join(names, ", ")+": "+c.text)
		}
	}
	return strings.Join(parts, "; ")
}

// GraphExportTool builds mark_graph_export, which takes no arguments.
func GraphExportTool(suffix string) mcp.Tool {
	return NewTool("mark_graph_export", describe(graphExportDescription, suffix))
}

// GraphPublishTool builds mark_graph_publish; targetDesc describes the graph document argument.
func GraphPublishTool(targetDesc, suffix string) mcp.Tool {
	return NewTool("mark_graph_publish",
		describe(graphPublishDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(targetDesc)),
		mcp.WithNumber("expected_version", mcp.Required(), mcp.Description(ExpectedVersionDesc)),
		mcp.WithNumber("retention", mcp.Description(graphRetentionDesc)),
	)
}

// budgetParam is the lookup argument for expanded sections, in result tokens.
const budgetParam = "budget"

// BudgetParam declares the budget argument on a lookup tool.
func BudgetParam() mcp.ToolOption {
	return mcp.WithNumber(budgetParam, mcp.Description(budgetDesc))
}

// Budget reads the budget argument as bytes; zero when absent or not positive.
func Budget(req *mcp.CallToolRequest) int {
	if n := req.GetInt(budgetParam, 0); n > 0 {
		return n * lookupexpand.BytesPerToken
	}
	return 0
}

// LookupParams are the lookup arguments both surfaces share after url.
func LookupParams() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("query", mcp.Required(), mcp.Description(QueryDesc)),
		mcp.WithString("filter", mcp.Description(FilterDesc)),
		mcp.WithNumber("limit", mcp.Description(LimitDesc)),
		mcp.WithString("match", mcp.Description(MatchDesc)),
		BudgetParam(),
		Lookup.Param(),
	}
}

// DoctorTool builds mark_doctor; urlDesc describes the scope argument.
func DoctorTool(urlDesc, suffix string) mcp.Tool {
	return NewTool("mark_doctor",
		describe(doctorDescription, suffix),
		mcp.WithString("url", mcp.Required(), mcp.Description(urlDesc)),
		mcp.WithBoolean("deep", mcp.Description(doctorDeepDesc)),
		mcp.WithBoolean("promotion", mcp.Description(doctorPromotionDesc)),
	)
}

// Tool profiles. Lean omits the operator and federation tools that no plugin
// prompt references; full is the complete surface for direct consumers.
const (
	ProfileLean = "lean"
	ProfileFull = "full"
)

type toolClass struct {
	write    bool // changes a store or the graph store
	advanced bool // absent from the lean profile
}

// toolClasses is every mark_* tool any surface serves. mark_archive stays in
// lean: /soul-archive is the only archive path a plugin user has.
var toolClasses = map[string]toolClass{
	"mark_fetch": {}, "mark_explore": {}, "mark_list": {}, "mark_versions": {},
	"mark_lookup": {}, "mark_lookup_all": {}, "mark_backlinks": {}, "mark_worlds": {},
	"mark_doctor": {}, "mark_discover": {advanced: true}, "mark_graph_export": {advanced: true},
	"mark_publish": {write: true}, "mark_append": {write: true}, "mark_archive": {write: true},
	"mark_curate": {write: true}, "mark_promote": {write: true}, "mark_graph": {write: true},
	"mark_graph_publish": {write: true, advanced: true},
}

// KnownTools and AdvancedTools are views of toolClasses; the plugin-prompts
// check fails when a template names an unknown or advanced tool.
var (
	KnownTools    = toolSet(func(toolClass) bool { return true })
	AdvancedTools = toolSet(func(c toolClass) bool { return c.advanced })
)

func toolSet(keep func(toolClass) bool) map[string]bool {
	set := map[string]bool{}
	for name, class := range toolClasses {
		if keep(class) {
			set[name] = true
		}
	}
	return set
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
