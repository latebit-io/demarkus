# Changelog

## 0.13.165

`/soul-doctor` and `/promote-scan` call the new `mark_doctor` tool instead of the plugin binary: the server reads the scope once and returns the hygiene report, so a bound broker store can be audited too, and `promotion: true` lists promotion candidates with the already-promoted set excluded. The style rules every publish is held to now run inside `mark_publish` on every surface; the Claude Code hook is unchanged. `/soul-journal` is one `mark_append` call with `route: "today"`: the server picks the day, creates or appends, widens tags and keeps the hubs linked, answering `present` for a repeated entry. `/soul-context` reads one primer, `/<slug>/.context` (hub, newest journal entries, roadmap, plans). Layout names come from the world's `/.well-known/demarkus/template.md` (`journal:`, `plans:`, `roadmap:`, `staging:`, `promote_types:`, `promote_floor:`); the seeded memory template carries the block.

`/soul-curate` is two `mark_curate` calls: the report (findings, the proposed edit or the split into a hub plus topic files with anchors kept, the inbound anchored links to repoint, a proposal hash) and, after the human gate, `apply: true` with that hash; the tool preflights every topic path and writes the hub last at the reported version. `/soul-archive` is one `mark_archive` call: a document, or a directory with `recursive: true`; the tool archives every live document under it and drops the hub bullets that only linked what was archived. The style gate now warns on any document at or over the 8 KB fetch threshold, not only a hub; journals are exempt.

`/promote`, `/soul-refresh` and the knowledge plugin's `knowledge-promote` skill run on the new `mark_promote` tool. The soul server stages the source (`stage`: markers, whether the layout marks it as shared, a packet), writes the back-stamp (`stamp`, marker-only by default, `stub` on request) and refreshes a promoted copy from the live knowledge body (`refresh`: in-sync, stale with the proposed body and a packet, behind). The knowledge server checks the distilled draft (`dry_run`: write grant, policy axes and fields, style, dedup by title and tags with candidate bodies) and publishes it with `promoted-from` provenance (`apply`), each write bound to the packet of the report before it. The memory broker serves the soul phases only; promotion stays client composed.

## 0.13.160

Recall guidance budgets by question shape: an answer in one section omits `budget` and fetches the section; an answer spanning documents or their relations passes `budget: 1500` on the first lookup and cites the appended blocks. The rule lives in the MCP server instructions; the skill and command defer to it.

## 0.13.157

mark_fetch and mark_explore return every metadata key on every call; the verbose argument is gone and no fetch instruction passes it.

## 0.13.143

`/soul-archive` runs the checkout-identity check before its first read: a new project has nothing to archive, an unknown identity stops, and a target outside the project's subtree needs the user's confirmation.

## 0.13.140

New `/soul-archive <path or dir/>`: archives one document or every live document under a directory (history kept, hidden from lists and lookup) and drops the hub bullets that linked them, behind a human gate. `mark_archive` joins the lean tool profile so the plugin's MCP server exposes it. `/soul-doctor` skips archived documents in its hygiene checks, reports a link to an archived document as broken with the fix "drop the link", and names `/soul-archive` as the other fix for an orphan.

## 0.13.129

/soul-doctor runs as one `demarkus-plugin doctor` call: the binary reads the scope once and derives every hygiene check (links, orphans, hubs, metadata, shape, style, ADR sequence and references, write-auth drift), exact within stated bounds; the prompt shrinks to the command and how to present the report.

## 0.13.127

Command and skill bodies cut about a third on invocation: the remember skill, /soul-journal, /soul-context, /soul and /soul-doctor share one store-resolution block backed by the new `registry project` subcommand (slug, store, binding state in one call), the doctor report is a pattern plus examples, and every step, bound and failure line is kept.

## 0.13.125

Session guidance cut to the behavioral contract, about a quarter of its former size: the plugin binary now states the project slug and bound store as a header line, and rules the write-time gates already enforce (destination, tags, retention, style) are stated once as follow-the-gate; the recall contract rides the MCP server instructions and the full layout and flows stay in the remember skill.

## 0.13.108

Default recall to scoped lookup followed by a targeted section fetch. Start with three results and no expansion; budgeted context remains optional for focused evidence queries.

## 0.13.100

Hub size gate: the style gate warns when a hub (`index.md` at any depth, or any link page) is at or over 8 KB, has a bullet past one line or carrying status (bold, `Status:`, a date, a PR number), or links over 40 documents. `/soul-doctor` gains hub shape, oversized-document, and document-shape findings; the remember skill's hub rule names the limits. Document shape: the style gate also warns on a missing summary under the H1 and on a heading carrying a date, a PR number, or a status word. New `/soul-curate <path>`: checks one document against the style rules and, behind a human gate, adds the summary, renames status headings, or splits a document over 8 KB into a hub plus topic files with anchors preserved. Task context: `mark_lookup` takes `budget` (approximate result tokens) and appends the matched sections' text in rank order, so one call answers a task; the same on the broker's `mark_lookup` and `mark_lookup_all`.

## 0.13.94

Lean MCP envelope: `mark_fetch` and `mark_explore` return status, version, and title by default; `mark_lookup` rows show ten tags then `+N more`. `verbose: true` restores every metadata key and full tag lists. Every prompt step that republishes a fetched document now fetches with `verbose: true` so the metadata map is preserved. Session guidance now steers reads to `match: body` and section anchors.

## 0.13.92

Frontmatter descriptions containing a colon are now quoted so strict YAML parsers load them; unquoted, Cursor (js-yaml) dropped `/soul-doctor`, `/soul-refresh`, and `/promote` from the slash menu. The generator now rejects frontmatter that is not valid YAML.

## 0.13.90

`remember` skill: hub rule for `index.md` and any link page (links plus one line each, nothing copied from children, children link back, about 40 outbound documents, split an outgrown document into a hub plus topic files).

## 0.13.89

Docs follow the vocabulary: "memory" is the store concept, "soul" is your own instance. README and manifest descriptions say soul where they mean the bound store.

## 0.13.87

Slash commands renamed from `/memory-*` back to `/soul-*` (`/soul`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-refresh`) and prose now says "soul" for the personal store, so it is not confused with the host's built-in memory feature. No aliases for the old names. Plugin name, MCP server id, and `mark_*` tools unchanged. Prompt templates trimmed for token cost.

## 0.13.76

Initial Cursor port of demarkus-memory. Cursor Plugin format with bash hook shims over the shared `demarkus-plugin` binary: sessionStart guidance and provisioning, a beforeMCPExecution publish tag-gate and destination gate (deny, ask, or allow with a warning), the ADR promote nudge, a sentinel-driven stop journal nudge, generated `/memory-*` commands and the `remember` skill, and `registry mcp ... --harness cursor` for joined memories. The recall nudge is not ported: Cursor's beforeSubmitPrompt hook cannot inject context.
