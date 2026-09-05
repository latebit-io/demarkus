# demarkus-knowledge for Cursor

Connect [Cursor](https://cursor.com) to an **organizational demarkus knowledge system**: a shared, versioned markdown catalog fronted by a demarkus-knowledge-broker over HTTPS. The Cursor port of the Claude Code [`demarkus-knowledge`](../claude-code-knowledge) plugin: same behavior, mapped onto Cursor's plugin, hook, and MCP surfaces. It shares `~/.demarkus` state with the Claude Code, pi, and OpenCode knowledge plugins, so one join covers every harness on the machine.

This is the broker-fronted, multi-writer counterpart to [`demarkus-memory` for Cursor](../cursor-memory), which gives you a personal soul. The two are independent plugins and compose: install both and the soul is your private scratch space while the knowledge system is the shared, authoritative source of truth. Install only this one to work against your org's catalog without running a local server.

## What it does

- **`/knowledge-join <broker-url>`**: validate an org broker (RFC 9728 metadata check), derive a server slug, register it as an HTTP MCP server in `~/.cursor/mcp.json` (`demarkus-plugin registry mcp --harness cursor add-http`), and record it so the publish gate covers it. Auth is Cursor's own MCP OAuth against the broker; no tokens to paste or store locally.
- **`/knowledge`**: list the systems you've joined and show each one's `root` hub index. The counterpart to `/soul`.
- **`/knowledge-doctor`**: read-only hygiene audit of a joined system, per world (orphans, broken links, dangling references, untagged and policy-noncompliant docs, ADR gaps).
- **`knowledge-promote` skill**: the curation cascade that lands a staged document in the catalog: triage, distill for a shared audience, dedup, tag to the system taxonomy, route to a writable world, human gate, publish with provenance. The memory plugin's `/promote` invokes it and applies the back-stamp; this side never writes the soul.
- **Standing guidance (`sessionStart`)**: when a system is joined, each conversation is steered to consult the shared catalog first for shared subjects, navigate via the `root` hub and `mark://<world>/<path>` URLs, prefer the broker-wide `mark_lookup_all`, and (when a soul also exists) keep the soul and system lanes.
- **Publish gate (`beforeMCPExecution`)**: enforces `tags` (and any required tag axes and OKF fields the system's policy declares) on writes to a joined system, at the severity the system chooses (`warn` / `block` / `ask`; Cursor supports `ask` natively). It gates **only** registered knowledge systems; the local soul and unrelated demarkus servers are left untouched.

## Requirements

`bash`, `curl`, `tar`, and `sha256sum` or `shasum` on PATH (the bootstrap uses them). macOS and Linux.

## Install

From the Cursor marketplace, once listed: open **Customize** in the sidebar, find `demarkus-knowledge`, and install at user scope.

From this repository as a team marketplace (Teams and Enterprise plans): add `latebit-io/demarkus` as a marketplace source; the manifest lives at `.cursor-plugin/marketplace.json`.

Locally, from a checkout:

```bash
git clone https://github.com/latebit-io/demarkus
ln -s "$PWD/demarkus/plugins/cursor-knowledge" ~/.cursor/plugins/local/demarkus-knowledge
```

Restart Cursor, then join a system:

```text
/knowledge-join https://mcp.broker.your-org.com
```

Restart Cursor once more and complete OAuth when prompted under Settings → MCP. `/knowledge` lists joined systems and orients you on each `root` hub.

### Update

Marketplace installs refresh from the repository (enable **Auto Refresh** in marketplace settings, or click **Refresh**). A local symlink follows `git pull`.

## Local helper

This plugin runs no local demarkus server. It bootstraps the shared `demarkus-plugin` helper for gates, guidance, and registry behavior, and reads the same registry and per-system policy mirror the other knowledge plugins write under `~/.demarkus/`:

- `~/.demarkus/knowledge-systems`: one joined MCP server slug per line.
- `~/.demarkus/plugin-knowledge.strictness.<slug>`, `.require-tags.<slug>`, `.require-fields.<slug>`: mirrored per-system gate policy.

It owns the `plugin-knowledge.*` namespace and never writes the memory plugin's files; it only checks whether a soul is configured so the soul and system guidance appears when relevant.

## Architecture

- `hooks/hooks.json` and `hooks/*.sh`: zero-logic bash shims. Each pipes Cursor's hook payload to `~/.demarkus/bin/demarkus-plugin` with `--format cursor` and prints the binary's answer. They fail open when the binary is absent. `hooks/test.sh` is their contract test (a fake binary records the calls).
- `scripts/bootstrap.sh`: installs the pinned `demarkus-plugin` binary; byte-identical across every plugin.
- No bundled `mcp.json`: joined systems are remote HTTP servers registered per machine in `~/.cursor/mcp.json` by `/knowledge-join`.
- `commands/*.md`, `context/`, and `skills/`: generated distribution artifacts. Edit the monorepo's `plugins/prompt-source/`, then run `cd tools && go run ./plugin-prompts write`.

### Behavior mapping (Claude Code → Cursor)

| Claude Code hook | Cursor |
|---|---|
| `SessionStart` (bootstrap, guidance) | `sessionStart` → `additional_context` |
| `PreToolUse` publish gate (deny, ask) | `beforeMCPExecution` → `permission: deny` or `ask` |
| gate `warn` (PostToolUse) | same hook → `permission: allow` plus `agent_message` |
| `UserPromptSubmit` recall-nudge | not available: `beforeSubmitPrompt` cannot inject context |
| `claude mcp add --transport http` | `registry mcp --harness cursor add-http` → `~/.cursor/mcp.json` |

Cloud agents run no session or MCP hooks and do not read the user-scope `~/.cursor/mcp.json`, so this plugin is for the desktop IDE. No `rules/` fallback ships: an `alwaysApply` rule would cost context on every request in the IDE to reach agents that cannot see the joined server anyway.

## Admin: publishing conventions

A knowledge system's org-wide policy and per-world template live on its `root` hub under `mark://root/.well-known/demarkus/`. See the Claude Code plugin's [`examples/knowledge-system/`](../claude-code-knowledge/examples/knowledge-system/) for the format an admin publishes; joined agents fetch and follow them and mirror the enforceable knobs into the local gate.

## Syncing with the other plugins

This package is developed in the [demarkus monorepo](https://github.com/latebit-io/demarkus) under `plugins/cursor-knowledge/`. Binary pins move with the pin-bump workflow; the plugin version follows the Claude Code knowledge plugin's version line.
