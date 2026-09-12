# Changelog

## 0.5.109

Default recall to scoped lookup followed by a targeted section fetch. Preserve world scope and partial-result disclosure; expand context only for focused evidence queries.

## 0.5.102

Hub size gate: the style gate warns when a hub (`index.md` at any depth, or any link page) is at or over 8 KB, has a bullet past one line or carrying status (bold, `Status:`, a date, a PR number), or links over 40 documents. `/knowledge-doctor` gains hub shape, oversized-document, and document-shape findings; session guidance names the rules. The style gate also warns on a missing summary under the H1 and on a heading carrying a date, a PR number, or a status word. Promote dedup: the cascade's step 3 is now two lookups (body match on the title, then tags) with bodies fetched only for candidates, replacing the exhaustive inventory fetch. Task context: `mark_lookup` and `mark_lookup_all` take `budget` (approximate result tokens) and append the matched sections' text in rank order, so one call answers a task.

## 0.5.97

Lean MCP envelope: `mark_fetch` and `mark_explore` return status, version, and title by default; `mark_lookup` rows show ten tags then `+N more`. `verbose: true` restores every metadata key and full tag lists. Every prompt step that republishes a fetched document now fetches with `verbose: true` so the metadata map is preserved. Session guidance now steers reads to `match: body` and section anchors.

## 0.5.95

Frontmatter descriptions containing a colon are now quoted so strict YAML parsers load them; unquoted, Cursor (js-yaml) dropped `/knowledge-doctor` and the `knowledge-promote` skill. The generator now rejects frontmatter that is not valid YAML.

## 0.5.83

- Guidance and commands now say "memory" for the personal store (formerly "soul") and point at the renamed `/memory-*` commands.

## 0.5.66

- Prefer broker-wide `mark_lookup_all` for shared catalog recall, with fallback for older brokers and plain endpoints.

## 0.5.55

- Replace the shortened local prompt copies with generated shared guidance and the full promotion cascade.
- Share hardened join and bounded multi-world doctor workflows across harnesses.
- Add real generated-command and installer coverage.

## 0.5.44

- Initial OpenCode port of the Claude Code knowledge plugin.
- Remote broker MCP registration through the shared endpoint catalog and OpenCode OAuth.
- Standing guidance, recall nudge, policy gate, slash commands, and native promotion skill.
- Gate ownership coordinates with the OpenCode memory plugin to prevent duplicate enforcement.
