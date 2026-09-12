# Changelog

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
