# demarkus-knowledge for Codex

Join an organizational demarkus **knowledge system** — a shared, broker-fronted, versioned markdown catalog — from the [OpenAI Codex CLI](https://developers.openai.com/codex/). Same behavior as the Claude Code and pi demarkus-knowledge plugins; it shares the exact same decision engine. No local server, no binaries beyond the shared `demarkus-plugin` (used only for the gate / guidance / recall logic). Pure broker + OAuth.

## What it does

- `/knowledge-join <broker-url>` validates an org's demarkus-broker, registers it as a Codex MCP server (`codex mcp add` + `codex mcp login`), and records it for the gate.
- Injects standing guidance at session start steering the agent to consult the shared catalog first and record durable shared knowledge there.
- Enforces a **publish tag-gate** (tags + `importance` + the system's required tag axes / metadata fields) on writes to a joined system, denying a bad write before it lands.
- Nudges recall on "what's the standard / did the team decide" prompts.
- Ships `/knowledge`, `/knowledge-join`, `/knowledge-doctor` as Codex **skills** (invoke with `/skills`, a `$knowledge`-style mention, or let Codex pick them implicitly).

## Architecture — adapter only

All decision **logic** lives in one shared Go binary, `demarkus-plugin` (installed to `~/.demarkus/bin` by `scripts/bootstrap.sh`, shipped in the demarkus `tools/` release), identical across harnesses. This plugin is a thin **adapter**:

- **Hooks** are declared in `config.example.toml` (`[[hooks.*]]`), installed into `~/.codex/config.toml`.
- **Hook scripts** in `hooks/` pipe Codex's hook JSON to `demarkus-plugin <cmd> --format codex*`. Codex's hook stdin/output schema matches Claude Code's, so the adapters are one-liners.
- **Prompt content** (`skills/`, `context/session-guidance.md`) is the same markdown as the other plugins (shipped as Codex skills), with the MCP-registration steps re-expressed in Codex's `codex mcp` CLI.

### Behavior mapping (Claude Code → Codex)

| Concern | Claude Code hook | Codex hook | Binary call |
|---|---|---|---|
| Guidance | `SessionStart` | `SessionStart` | `guidance --surface knowledge --format codex` |
| Publish tag-gate | `PreToolUse` | `PreToolUse` | `gate --format codex-pre` |
| Tag reminder (warn) | `PostToolUse` | `PostToolUse` | `gate --format codex-post` |
| Recall nudge | `UserPromptSubmit` | `UserPromptSubmit` | `nudge --event recall --surface knowledge --format codex` |

Codex's `PreToolUse` has no `ask` decision (that lives in its separate `PermissionRequest` event), so the binary's `codex-pre` format folds an `ask` into a `deny` whose reason tells the agent to confirm with the user first.

## Requirements

- Codex CLI with hooks support and `codex mcp` (add/login/list/remove).
- `curl`, `tar`, `install(1)`, and `sha256sum`/`shasum` (the bootstrap preflights these).
- macOS or Linux (arm64/amd64).

## Install

From a checkout of this repo:

```bash
plugins/codex-knowledge/install.sh
```

This installs the shared binary, writes a marker-delimited managed block into `~/.codex/config.toml` (the gate/guidance/recall hooks), and installs the `/knowledge*` skills into `~/.agents/skills/`. Restart Codex, then join a system with the knowledge-join skill (`/skills` → `knowledge-join`, or `$knowledge-join`):

```
$knowledge-join https://mcp.broker.acme.com
```

### Uninstall

Delete the block between the two `demarkus-knowledge (managed by install.sh)` markers in `~/.codex/config.toml`, remove the `knowledge*` skill directories from `~/.agents/skills/`, and for each joined system run `codex mcp logout/remove <slug>` and `demarkus-plugin registry knowledge-unregister <slug>`.

## Notes

- The gate (PreToolUse `deny`) is the enforced core and maps cleanly to Codex.
- The knowledge registry and policy mirrors under `~/.demarkus` are shared with the Claude Code and pi plugins on the same machine.
