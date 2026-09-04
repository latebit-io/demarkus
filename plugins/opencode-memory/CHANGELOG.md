# Changelog

## 0.13.86

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
