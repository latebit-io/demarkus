# Changelog

## 0.13.110

Default recall to scoped lookup followed by a targeted section fetch. Start with three results and no expansion; budgeted context remains optional for focused evidence queries.

## 0.13.102

Hub size gate: the style gate warns when a hub (`index.md` at any depth, or any link page) is at or over 8 KB, has a bullet past one line or carrying status (bold, `Status:`, a date, a PR number), or links over 40 documents. `/soul-doctor` gains hub shape, oversized-document, and document-shape findings; the remember skill's hub rule names the limits. Document shape: the style gate also warns on a missing summary under the H1 and on a heading carrying a date, a PR number, or a status word. New `/soul-curate <path>`: checks one document against the style rules and, behind a human gate, adds the summary, renames status headings, or splits a document over 8 KB into a hub plus topic files with anchors preserved. Task context: `mark_lookup` takes `budget` (approximate result tokens) and appends the matched sections' text in rank order, so one call answers a task; the same on the broker's `mark_lookup` and `mark_lookup_all`.

## 0.13.96

Lean MCP envelope: `mark_fetch` and `mark_explore` return status, version, and title by default; `mark_lookup` rows show ten tags then `+N more`. `verbose: true` restores every metadata key and full tag lists. Every prompt step that republishes a fetched document now fetches with `verbose: true` so the metadata map is preserved. Session guidance now steers reads to `match: body` and section anchors.

## 0.13.94

Frontmatter descriptions containing a colon are now quoted so strict YAML parsers load them; unquoted, Cursor (js-yaml) dropped `/soul-doctor`, `/soul-refresh`, and `/promote` from the slash menu. The generator now rejects frontmatter that is not valid YAML.

## 0.13.92

`remember` skill: hub rule for `index.md` and any link page (links plus one line each, nothing copied from children, children link back, about 40 outbound documents, split an outgrown document into a hub plus topic files).

## 0.13.91

Docs follow the vocabulary: "memory" is the store concept, "soul" is your own instance. README and manifest descriptions say soul where they mean the bound store.

## 0.13.89

Slash commands renamed from `/memory-*` back to `/soul-*` (`/soul`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-refresh`) and prose now says "soul" for the personal store, so it is not confused with the host's built-in memory feature. No aliases for the old names. Plugin name, MCP server id, and `mark_*` tools unchanged. Prompt templates trimmed for token cost.

## 0.13.77

- Remove the deprecated `/soul-*` command aliases (`/soul`, `/soul-init`, `/soul-join`, `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-refresh`, `/soul-default`); use the `/memory-*` names. The pinned binary is unchanged and still accepts its `soul-*` helper names.

## 0.13.76

- Call the canonical `memory-*` helper names now that the pinned binary (tools 0.27.0) provides them: `registry memory-join`, `registry memory-default`, `mcp-serve --memory`, and the `memoryWrite` session-end signal. The `soul-*` names stay accepted by the binary for one more release; MCP entries registered earlier with `--soul` keep working.

## 0.13.73

- Rename the personal store from "soul" to "memory" across guidance, commands, and the skill: `/soul-*` commands become `/memory-*` (`/memory`, `/memory-init`, `/memory-join`, `/memory-context`, `/memory-journal`, `/memory-status`, `/memory-doctor`, `/memory-refresh`, `/memory-default`); the `soul-memory` skill becomes `memory`. The old command names keep working as deprecated aliases for one release. On-disk state under `~/.demarkus` is unchanged.

## 0.13.42

- Generate all runtime prompts from the shared `plugins/prompt-source` corpus.
- Share hardened metadata, join, doctor, and status guidance with every harness.
- Add real generated-command tests and foreign-harness leakage checks.

## 0.13.29

- Register as the shared OpenCode gate owner so the memory and knowledge plugins can coexist without duplicate policy decisions or warnings.
- Log fail-open helper failures and ignore synthetic guidance when detecting recall prompts.
- Treat the tools pin as a minimum so an older co-installed plugin cannot downgrade a newer helper binary.

## 0.13.27

- Update check on plugin init: `demarkus-plugin update-check` compares the installed version against the manifest on `main`, and the adapter toasts when a newer release exists (notify-only, never self-installing). Throttled to once per 24h by the binary, silent when offline, turned off with `DEMARKUS_UPDATE_CHECK=0` or `~/.demarkus/plugin.update-check`. Requires a tools release carrying `update-check`; until the pin moves, the check is a no-op.
- `install.sh` installs `package.json` alongside the assets, so the plugin can read the version it was installed at.

## 0.13.8

- Initial OpenCode port of the Claude Code `demarkus-memory` plugin (version tracks the plugin line for the shared pin-bump convention).
- Thin TypeScript adapter over the shared `demarkus-plugin` binary: publish tag-gate + destination gate (`tool.execute.before`), warn + promote nudges (`tool.execute.after`), standing guidance + recall nudge (`chat.message`), journal nudge (`session.idle` toast), MCP + command registration (`config` hook).
- Installed via `install.sh` into OpenCode's global plugins directory; no npm.
