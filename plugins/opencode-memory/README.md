# demarkus-opencode-memory

Local, versioned project memory for [OpenCode](https://opencode.ai) via [demarkus](https://github.com/latebit-io/demarkus). The OpenCode port of the Claude Code `demarkus-memory` plugin; same behavior, mapped onto OpenCode's plugin hooks. It shares `~/.demarkus` state with the Claude Code and pi plugins, so all three coexist on one machine: one memory, one token, one set of registries.

## What it does

- **Zero-config provisioning.** On startup it downloads the pinned demarkus binaries, generates a `0600` capability token, spawns a managed local `demarkus-server`, and registers the `demarkus-memory` MCP server in OpenCode's config.
- **Standing guidance.** Injects "recall first, record as you go" guidance once per session so the agent self-documents to the memory.
- **Publish tag-gate.** A tagless `mark_publish` is invisible to `mark_lookup` forever; the gate makes that loud at write time (`warn` by default, `block`/`ask` available).
- **Destination gate.** When a repo is bound to a specific memory, a write aimed at a different memory is denied so memory lands on the right store.
- **Recall / journal / promote nudges.** Discreet reminders at the moments they matter.
- **Slash commands.** `/memory`, `/memory-context`, `/memory-journal`, `/memory-init`, `/memory-join`, `/memory-default`, `/memory-status`, `/memory-doctor`, `/memory-refresh`, `/promote`, `/promote-scan`, plus the on-demand `memory` skill (native OpenCode skill).

## Requirements

- `bash`, `curl`, `tar` on PATH (used by the bundled provisioning scripts).

## Install

No npm involved; OpenCode auto-loads plugin files from its global plugins directory. One-liner (downloads the plugin from the repo):

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

Or from a checkout:

```bash
git clone https://github.com/latebit-io/demarkus
demarkus/plugins/opencode-memory/install.sh
```

This copies the plugin to `$XDG_CONFIG_HOME/opencode/plugins/demarkus-memory.ts` (default `~/.config/opencode/plugins/`), the skill to the sibling `skills/memory/`, and the assets it reads to `~/.demarkus/opencode-memory/`. Start (or restart) OpenCode; the first session provisions the memory. Restart once after the first session so the newly-registered MCP server connects. Diagnose with `/memory-status`, reconfigure with `/memory-init`, remove with `install.sh --uninstall`.

### Update

Re-run the installer; it is idempotent and stages before it commits, so a failed update leaves the previous install intact:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
# or, from a checkout
git -C demarkus pull && demarkus/plugins/opencode-memory/install.sh
```

`DEMARKUS_REF=<branch|tag|sha>` pins the git ref for the standalone install. Restart OpenCode afterwards; the managed binaries update themselves on the next session from the pins the new plugin carries.

### Update check

On plugin init the adapter reads its version from the `package.json` that `install.sh` copies into `~/.demarkus/opencode-memory/`, and hands it to `demarkus-plugin update-check` together with this plugin's manifest URL and update command; the binary compares against the manifest on `main` and the adapter toasts whatever it returns. Notify-only: nothing installs itself, since the plugin file is already loaded and a rewrite could not take effect until the next restart. The binary throttles to one check per 24h (stamp: `~/.demarkus/.update-check-demarkus-opencode-memory`), the call is fire-and-forget so it cannot delay startup, and it stays silent when offline or before provisioning has installed the binary. Turn it off with `DEMARKUS_UPDATE_CHECK=0` or by writing `0` to `~/.demarkus/plugin.update-check`. A `DEMARKUS_REF`-pinned install still compares against `main`, so a pinned older revision reports an update.

## Architecture

- `src/demarkus-memory.ts`: the whole adapter, one self-contained file (OpenCode's plugin dir loads flat files). All gate/nudge/guidance/update-check logic lives in the shared `demarkus-plugin` binary; the adapter hands it normalized JSON and applies the result. Fails open when the binary is absent.
- `scripts/bootstrap.sh`: pinned binary installer, reused verbatim from the Claude Code plugin (single source of truth for provisioning).
- `commands/*.md`, `context/`, and `skills/`: generated distribution artifacts from `plugins/prompt-source/`; run `cd tools && go run ./plugin-prompts write` after editing canonical prompts.

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
