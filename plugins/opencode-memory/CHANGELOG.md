# Changelog

## 0.13.27

- Update check on plugin init: `demarkus-plugin update-check` compares the installed version against the manifest on `main`, and the adapter toasts when a newer release exists (notify-only, never self-installing). Throttled to once per 24h by the binary, silent when offline, turned off with `DEMARKUS_UPDATE_CHECK=0` or `~/.demarkus/plugin.update-check`. Requires a tools release carrying `update-check`; until the pin moves, the check is a no-op.
- `install.sh` installs `package.json` alongside the assets, so the plugin can read the version it was installed at.

## 0.13.8

- Initial OpenCode port of the Claude Code `demarkus-memory` plugin (version tracks the plugin line for the shared pin-bump convention).
- Thin TypeScript adapter over the shared `demarkus-plugin` binary: publish tag-gate + destination gate (`tool.execute.before`), warn + promote nudges (`tool.execute.after`), standing guidance + recall nudge (`chat.message`), journal nudge (`session.idle` toast), MCP + command registration (`config` hook).
- Installed via `install.sh` into OpenCode's global plugins directory; no npm.
