# demarkus-opencode-memory

Local, versioned project memory for [OpenCode](https://opencode.ai) via [demarkus](https://github.com/latebit-io/demarkus). The OpenCode port of the Claude Code `demarkus-memory` plugin; same behavior, mapped onto OpenCode's plugin hooks. It shares `~/.demarkus` state with the Claude Code and pi plugins, so all three coexist on one machine: one memory, one token, one set of registries.

## What it does

- **Zero-config provisioning.** On startup it downloads the pinned demarkus binaries, generates a `0600` capability token, spawns a managed local `demarkus-server`, and registers the `demarkus-memory` MCP server in OpenCode's config.
- **Standing guidance.** Injects "recall first, record as you go" guidance once per session so the agent self-documents to the soul.
- **Publish tag-gate.** A tagless `mark_publish` is invisible to `mark_lookup` forever; the gate makes that loud at write time (`warn` by default, `block`/`ask` available).
- **Destination gate.** When a repo is bound to a specific soul, a write aimed at a different soul is denied so writes land on the right soul.
- **Recall / journal / promote nudges.** Discreet reminders at the moments they matter.
- **Slash commands.** `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-curate`, `/soul-archive`, `/soul-refresh`, `/promote`, `/promote-scan`, plus the on-demand `remember` skill (native OpenCode skill).

## Requirements

- OpenCode V2 with `opencode plugin add` and Git package subdirectory support.
- `bash`, `curl`, `tar` on PATH (used by the bundled provisioning scripts).

## Install

Install the package directly from the monorepo:

```bash
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
```

Start a session; the plugin provisions memory automatically. Diagnose with `/soul-status`, reconfigure with `/soul-init`. No separate installer or npm publication is needed. Existing script installs must [migrate first](MIGRATION.md).

For development, configure the checkout directory in `opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/absolute/path/to/demarkus/plugins/opencode-memory"]
}
```

OpenCode manages the package. Commands and skills register from its generated Markdown; guidance and bootstrap scripts resolve relative to the package, including paths containing spaces. User-defined commands and skills take precedence. Personal data, binaries and registries still live in `~/.demarkus`.

### Update

Use OpenCode's package manager:

```bash
opencode plugin check
opencode plugin update 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
```

Replace `main` with a full commit SHA to pin a revision. Full commit pins are not updated automatically. The bundled bootstrap retains the shared helper's minimum-version pin and never downgrades a newer helper.

### Remove

```bash
opencode plugin remove 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
```

Use the exact configured target if pinned. Removing the plugin leaves your soul, shared binaries, registries and OAuth credentials intact.

## Architecture

- `index.ts`: package entrypoint; exports only the plugin definition.
- `src/demarkus-memory.ts`: host adapter. Gate, nudge, guidance and provisioning logic lives in the shared `demarkus-plugin` binary.
- `src/package.ts`: generated-prompt registration, shared byte-for-byte with the knowledge package and checked by `scripts/check-identical-copies.sh`.
- `scripts/bootstrap.sh`: pinned binary installer, reused verbatim from the Claude Code plugin (single source of truth for provisioning).
- `commands/*.md`, `context/`, and `skills/`: generated distribution artifacts from `plugins/prompt-source/`; run `cd tools && go run ./plugin-prompts write` after editing canonical prompts.

Branding remains governed by [the brand generator](../brands/README.md). OpenCode is not a brandable base; package installation does not introduce another prompt source or branding mechanism.

### Behavior mapping (Claude Code → OpenCode)

| Claude Code hook | OpenCode |
|---|---|
| `SessionStart` provisioning | plugin init, fire-and-forget |
| `SessionStart` guidance | V2 model-only `session.context`; V1 `chat.message` |
| `UserPromptSubmit` recall-nudge | V2 `session.prompt` observes text, then `session.context` injects privately; V1 `chat.message` |
| `PreToolUse` publish/dest gate | V2 `tool.hook("execute.before")`; V1 `tool.execute.before` |
| gate `warn` (PostToolUse) | V2 `tool.hook("execute.after")`; V1 `tool.execute.after` |
| `PostToolUse` promote-nudge | V2/V1 after-hook on a fresh ADR publish |
| `Stop` journal-nudge | V2 queues model-only context for the next request; V1 toast, once per session |
| `.mcp.json` bundled MCP server | V2 `mcp.transform`; V1 `config.mcp` |
| `commands/*.md` | V2 command transform; V1 `config.command` |
| skill | V2 skill transform, reading the generated `SKILL.md` |

`ask` strictness maps to a block whose reason tells the agent to confirm with the user first (`tool.execute.before` has no native "ask").
