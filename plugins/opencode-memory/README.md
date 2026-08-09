# demarkus-opencode-memory

Local, versioned project memory (the "soul") for [OpenCode](https://opencode.ai) via [demarkus](https://github.com/latebit-io/demarkus). The OpenCode port of the Claude Code `demarkus-memory` plugin; same behavior, mapped onto OpenCode's plugin hooks. It shares `~/.demarkus` state with the Claude Code and pi plugins, so all three coexist on one machine: one soul, one token, one set of registries.

## What it does

- **Zero-config provisioning.** On startup it downloads the pinned demarkus binaries, generates a `0600` capability token, spawns a managed local `demarkus-server`, and registers the `demarkus-memory` MCP server in OpenCode's config.
- **Standing guidance.** Injects "recall first, record as you go" guidance once per session so the agent self-documents to the soul.
- **Publish tag-gate.** A tagless `mark_publish` is invisible to `mark_lookup` forever; the gate makes that loud at write time (`warn` by default, `block`/`ask` available).
- **Destination gate.** When a repo is bound to a specific soul, a write aimed at a different soul is denied so memory lands on the right store.
- **Recall / journal / promote nudges.** Discreet reminders at the moments they matter.
- **Slash commands.** `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-refresh`, `/promote`, `/promote-scan`, plus the on-demand `soul-memory` skill (native OpenCode skill).

## Requirements

- `bash`, `curl`, `tar` on PATH (used by the bundled provisioning scripts).

## Install

No npm involved; OpenCode auto-loads plugin files from its global plugins directory:

```bash
git clone https://github.com/latebit-io/demarkus
demarkus/plugins/opencode-memory/install.sh
```

This copies the plugin to `~/.config/opencode/plugins/demarkus-memory.ts`, the skill to `~/.config/opencode/skills/soul-memory/`, and the assets it reads to `~/.demarkus/opencode-memory/`. Start (or restart) OpenCode; the first session provisions the soul. Restart once after the first session so the newly-registered MCP server connects. Diagnose with `/soul-status`, reconfigure with `/soul-init`, remove with `install.sh --uninstall`.

Update by pulling the repo and re-running `install.sh`.

## Architecture

- `src/demarkus-memory.ts` — the whole adapter, one self-contained file (OpenCode's plugin dir loads flat files). All gate/nudge/guidance logic lives in the shared `demarkus-plugin` binary; the adapter hands it normalized JSON and applies the result. Fails open when the binary is absent.
- `scripts/bootstrap.sh` — pinned binary installer, reused verbatim from the Claude Code plugin (single source of truth for provisioning).
- `commands/*.md` — slash-command prompt bodies, registered via the `config` hook.
- `skills/soul-memory/` — the on-demand memory-routing skill, installed into OpenCode's native skills directory.

### Behavior mapping (Claude Code → OpenCode)

| Claude Code hook | OpenCode |
|---|---|
| `SessionStart` provisioning | plugin init, fire-and-forget |
| `SessionStart` guidance | `chat.message`, first message per session |
| `UserPromptSubmit` recall-nudge | `chat.message`, on the prompt text |
| `PreToolUse` publish/dest gate | `tool.execute.before` throwing to block |
| gate `warn` (PostToolUse) | `tool.execute.after` appends reminder to tool output |
| `PostToolUse` promote-nudge | `tool.execute.after`, on a fresh ADR publish |
| `Stop` journal-nudge | `event` on `session.idle`, toast, once per session |
| `.mcp.json` bundled MCP server | `config` hook sets `config.mcp` |
| `commands/*.md` | `config` hook sets `config.command` |
| skill | native `SKILL.md` in `~/.config/opencode/skills/` |

`ask` strictness maps to a block whose reason tells the agent to confirm with the user first (`tool.execute.before` has no native "ask").
