# demarkus-cowork

Local, versioned memory for [Claude Cowork](https://claude.com/product/cowork) via [demarkus](https://github.com/latebit-io/demarkus). It talks to the same `~/.demarkus` soul as the Claude Code, Cursor, OpenCode, and pi plugins: one store, one token.

## What differs from the Claude Code plugin

Cowork runs no plugin hooks, and its Bash tool runs in a sandbox with no access to `~/.demarkus`. This plugin is built around both limits:

- **MCP only.** The `demarkus-memory` MCP server runs on your machine, outside the sandbox. Its launcher installs the pinned binaries and attaches to (or starts) the managed local server, so no hook is needed.
- **No shell commands.** Prompts never run `demarkus-plugin`. `/soul-init`, `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-refresh`, `/promote`, and `/promote-scan` are not shipped; run them from Claude Code or a terminal.
- **No gates or nudges.** The tag gate, destination gate, and journal, recall, and promote nudges need hooks. Server-side write policy still applies.
- **Project slug.** There is no project directory or binding. The `remember` skill asks which project to use and never takes the slug from the sandbox working directory.

## Commands

`/soul`, `/soul-context`, `/soul-journal`, `/soul-archive`, `/soul-curate`, and the `remember` skill, which carries the standing "recall first, record as you go" guidance a hook would inject elsewhere.

## Requirements

Cowork with local MCP servers enabled; `bash`, `curl`, `tar`, and `sha256sum` or `shasum` on the host. macOS and Linux. Provision the soul once from Claude Code (`/soul-init`) or let the launcher do it on first use.

## Install

Upload the plugin as a zip (Customize, Plugins), or add this repository as a marketplace and install `demarkus-cowork`. Do not install it next to `demarkus-memory` in the same Cowork workspace: both register the same local MCP server.

Beside a branded Claude Code plugin on one machine, brand this plugin with the same `mcp_server_key`. `mcp-serve` records that key as the local memory's name for the gates, and a start without `--name` clears it, which would turn the gates off for the branded plugin.

## Development

The commands and skill under `commands/` and `skills/` are generated from `plugins/prompt-source` by `tools/plugin-prompts` (target `cowork-memory`). Edit the templates, then `cd tools && go run ./plugin-prompts write`. `scripts/bootstrap.sh` and `scripts/mcp-launch.sh` are byte copies of the Claude Code plugin's, checked by `scripts/check-identical-copies.sh`.
