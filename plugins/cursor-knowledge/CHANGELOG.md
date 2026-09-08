# Changelog

## 0.5.103

Hub size gate: the style gate warns when a hub (`index.md` at any depth, or any link page) is at or over 8 KB, has a bullet past one line or carrying status (bold, `Status:`, a date, a PR number), or links over 40 documents. `/knowledge-doctor` gains hub shape, oversized-document, and document-shape findings; session guidance names the rules. The style gate also warns on a missing summary under the H1 and on a heading carrying a date, a PR number, or a status word. Promote dedup: the cascade's step 3 is now two lookups (body match on the title, then tags) with bodies fetched only for candidates, replacing the exhaustive inventory fetch. Task context: `mark_lookup` and `mark_lookup_all` take `budget` (approximate result tokens) and append the matched sections' text in rank order, so one call answers a task.

## 0.5.99

Lean MCP envelope: `mark_fetch` and `mark_explore` return status, version, and title by default; `mark_lookup` rows show ten tags then `+N more`. `verbose: true` restores every metadata key and full tag lists. Every prompt step that republishes a fetched document now fetches with `verbose: true` so the metadata map is preserved. Session guidance now steers reads to `match: body` and section anchors.

## 0.5.97

Frontmatter descriptions containing a colon are now quoted so strict YAML parsers load them; unquoted, Cursor (js-yaml) dropped `/knowledge-doctor` and the `knowledge-promote` skill. The generator now rejects frontmatter that is not valid YAML.

## 0.5.95

Initial Cursor port of demarkus-knowledge. Cursor Plugin format with bash hook shims over the shared `demarkus-plugin` binary: sessionStart knowledge guidance, a beforeMCPExecution publish gate (tags, policy axes and fields; deny, ask, or allow with a warning) scoped to joined systems, generated `/knowledge`, `/knowledge-join`, `/knowledge-doctor` commands and the `knowledge-promote` skill, and `registry mcp --harness cursor add-http` for the broker's OAuth MCP entry in `~/.cursor/mcp.json`. The recall nudge is not ported: Cursor's beforeSubmitPrompt hook cannot inject context.
