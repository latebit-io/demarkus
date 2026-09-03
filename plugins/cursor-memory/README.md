# demarkus-memory for Cursor

Local, versioned project memory for [Cursor](https://cursor.com), via [demarkus](https://github.com/latebit-io/demarkus). The Cursor port of the Claude Code `demarkus-memory` plugin: same behavior, mapped onto Cursor's plugin, hook, and MCP surfaces. It shares `~/.demarkus` state with the Claude Code, pi, and OpenCode plugins, so all of them coexist on one machine: one memory, one token, one set of registries.

## What it does

- **Zero-config provisioning.** On each new conversation the `sessionStart` hook downloads the pinned demarkus binaries, generates a `0600` capability token, and spawns a managed local `demarkus-server`. The bundled `mcp.json` wires the `demarkus-memory` MCP server (`demarkus-plugin mcp-serve`).
- **Standing guidance.** "Recall first, record as you go" guidance is injected into every conversation so the agent self-documents to the memory.
- **Publish tag-gate.** A tagless `mark_publish` is invisible to `mark_lookup` forever; the `beforeMCPExecution` hook makes that loud at write time (`warn` by default, `block` and `ask` available; Cursor supports `ask` natively).
- **Destination gate.** When a repo is bound to a specific memory, a write aimed at a different memory is denied (default `block`).
- **Promote and journal nudges.** An ADR publish reminds you to `/promote` once a knowledge system is joined; a conversation that edited files without writing to the memory gets one follow-up nudge at stop.
- **Slash commands.** `/memory`, `/memory-context`, `/memory-journal`, `/memory-init`, `/memory-join`, `/memory-default`, `/memory-status`, `/memory-doctor`, `/memory-refresh`, `/promote`, `/promote-scan`, plus the on-demand `remember` skill. The MCP server's own prompts appear as `/demarkus-memory/orient`, `/demarkus-memory/recall`, and `/demarkus-memory/whats-new`.

## Requirements

`bash`, `curl`, `tar`, and `sha256sum` or `shasum` on PATH (the bootstrap uses them). macOS and Linux.

## Install

From the Cursor marketplace, once listed: open **Customize** in the sidebar, find `demarkus-memory`, and install at user scope.

From this repository as a team marketplace (Teams and Enterprise plans): add `latebit-io/demarkus` as a marketplace source; the manifest lives at `.cursor-plugin/marketplace.json`.

Locally, from a checkout:

```bash
git clone https://github.com/latebit-io/demarkus
ln -s "$PWD/demarkus/plugins/cursor-memory" ~/.cursor/plugins/local/demarkus-memory
```

Restart Cursor. The first conversation provisions the memory; if the `demarkus-memory` MCP server shows as failed before that (the binary is installed by the first `sessionStart`), toggle it once under Cursor Settings → MCP. Use `/memory-status` to diagnose and `/memory-init` to reconfigure (adopt an existing server, switch modes).

### Update

Marketplace installs refresh from the repository (enable **Auto Refresh** in marketplace settings, or click **Refresh**). A local symlink follows `git pull`. Restart Cursor after an update that changes the MCP wiring.

## Architecture

- `hooks/hooks.json` and `hooks/*.sh`: zero-logic bash shims. Each pipes Cursor's hook payload to `~/.demarkus/bin/demarkus-plugin` with `--format cursor` and prints the binary's answer. They fail open when the binary is absent. `hooks/test.sh` is their contract test (a fake binary records the calls).
- `scripts/bootstrap.sh`: installs the pinned `demarkus-plugin` binary; byte-identical across every plugin.
- `mcp.json`: the local memory server. Joined memories and knowledge systems are registered in `~/.cursor/mcp.json` by `demarkus-plugin registry mcp --harness cursor add|add-http|remove|list`.
- `commands/*.md`, `context/`, and `skills/`: generated distribution artifacts. Edit the monorepo's `plugins/prompt-source/`, then run `cd tools && go run ./plugin-prompts write`.

### Behavior mapping (Claude Code → Cursor)

| Claude Code hook | Cursor |
|---|---|
| `SessionStart` (provision, guidance) | `sessionStart` → `additional_context` |
| `PreToolUse` publish/dest gate (deny, ask) | `beforeMCPExecution` → `permission: deny` or `ask` |
| gate `warn` (PostToolUse) | same hook → `permission: allow` plus `agent_message` |
| `PostToolUse` promote-nudge | `beforeMCPExecution` on `mark_publish` → `agent_message` (the after-call hook cannot speak to the agent) |
| `UserPromptSubmit` recall-nudge | not available: `beforeSubmitPrompt` cannot inject context |
| `Stop` journal-nudge (transcript grep) | `afterFileEdit` and `afterMCPExecution` leave per-conversation sentinels; `stop` reads them and emits one `followup_message` (`loop_limit: 1`) |

Cloud agents run none of the MCP or session hooks and cannot reach a local stdio server, so this plugin is for the desktop IDE.

## Syncing with the other plugins

This package is developed in the [demarkus monorepo](https://github.com/latebit-io/demarkus) under `plugins/cursor-memory/`. Binary pins move with the pin-bump workflow; the plugin version follows the Claude Code memory plugin's version line.
