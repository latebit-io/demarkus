# demarkus-memory for Codex

Local, versioned project memory (a "soul") for the [OpenAI Codex CLI](https://developers.openai.com/codex/), backed by a local demarkus-server. Same behavior as the Claude Code and pi demarkus-memory plugins — it shares the exact same decision engine.

## What it does

- Provisions a local `demarkus-server`, auto-generates a token, and wires the `demarkus-memory` MCP server (`mark_fetch`/`mark_publish`/`mark_append`/`mark_graph`/`mark_lookup` over your own versioned markdown store).
- Injects standing guidance at session start so the agent self-documents to the soul.
- Enforces a **publish tag-gate** (every publish needs `tags`; `importance` in [0,1]) and a **destination gate** (a project bound to a soul can't write to a different one), denying a bad write before it lands.
- Nudges recall on "what did we decide" prompts, promotion on a fresh ADR publish, and journaling at session end.
- Ships the `/soul-*` and `/promote*` procedures as Codex **skills** (invoke with `/skills`, a `$soul-init`-style mention, or let Codex pick them implicitly).

## Architecture — adapter only

All decision **logic** lives in one shared Go binary, `demarkus-plugin` (installed to `~/.demarkus/bin` by `scripts/bootstrap.sh`, shipped in the demarkus `tools/` release). It is identical across harnesses; this plugin is a thin **adapter** that re-expresses Codex's wiring:

- **MCP server + hooks** are declared in `config.example.toml` (`[mcp_servers.demarkus-memory]` + `[[hooks.*]]`), installed into `~/.codex/config.toml`.
- **Hook scripts** in `hooks/` just pipe Codex's hook JSON to `demarkus-plugin <cmd> --format codex*` and let it emit the Codex-native decision. Codex's hook stdin (`tool_name`, `tool_input`, `cwd`, `prompt`, `stop_hook_active`, `transcript_path`) and output schema (`hookSpecificOutput.permissionDecision` / `additionalContext`, `{decision,reason}`) match Claude Code's, so the adapters are one-liners.
- **Prompt content** (`skills/`, `context/session-guidance.md`) is the same markdown as the other plugins (the command procedures shipped as Codex skills).

A fix in the binary fixes every harness at once; a new harness reimplements nothing.

### Behavior mapping (Claude Code → Codex)

| Concern | Claude Code hook | Codex hook | Binary call |
|---|---|---|---|
| Provision + guidance | `SessionStart` | `SessionStart` | `provision`, `guidance --format codex` |
| Publish/destination gate | `PreToolUse` | `PreToolUse` | `gate --format codex-pre` |
| Tag reminder (warn) | `PostToolUse` | `PostToolUse` | `gate --format codex-post` |
| Promote nudge | `PostToolUse` | `PostToolUse` | `nudge --event promote --format codex` |
| Recall nudge | `UserPromptSubmit` | `UserPromptSubmit` | `nudge --event recall --format codex` |
| Journal nudge | `Stop` | `Stop` | `nudge --event session-end --format codex` |

Codex's `PreToolUse` has no `ask` decision (that lives in its separate `PermissionRequest` event), so the binary's `codex-pre` format folds an `ask` into a `deny` whose reason tells the agent to confirm with the user first.

## Requirements

- Codex CLI with hooks support (config.toml `[[hooks.*]]`).
- `curl`, `tar`, `install(1)`, and `sha256sum`/`shasum` (the bootstrap preflights these).
- macOS or Linux (arm64/amd64).

## Install

From a checkout of this repo:

```bash
plugins/codex-memory/install.sh
```

This installs the shared binary, writes a marker-delimited managed block into `~/.codex/config.toml` (MCP server + hooks), and installs the `/soul-*` and `/promote*` skills into `~/.agents/skills/`. Re-running replaces the managed block in place. Restart Codex (or start a new session) to load it, then invoke a skill with `/skills`, `$soul-init`, etc.

To wire it by hand instead, see `config.example.toml` (replace `@BIN@` with `~/.demarkus/bin/demarkus-plugin` and `@PLUGIN_DIR@` with this directory).

### Uninstall

Delete the block between the two `demarkus-memory (managed by install.sh)` markers in `~/.codex/config.toml`, and remove the `soul-*`/`promote*` skill directories from `~/.agents/skills/`. The shared binary and your soul data under `~/.demarkus` are left intact.

## Notes

- The gate (PreToolUse `deny`) is the enforced core and maps cleanly to Codex. The session-end journal nudge reads the transcript to detect file changes / soul writes; its patterns cover Codex's `apply_patch`/`shell`/`mcp__*` markers, and a missed signal degrades to "no nudge", never a false block.
- The soul, server, and binary under `~/.demarkus` are shared with the Claude Code and pi plugins on the same machine — joining a soul or setting policy in one is visible to all.
