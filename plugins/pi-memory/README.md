# demarkus-pi-memory

Local, versioned project memory (the "soul") for the [pi](https://pi.dev) coding agent, via [demarkus](https://github.com/latebit-io/demarkus). The pi port of the Claude Code `demarkus-memory` plugin — same behavior, mapped onto pi's extension API. It shares `~/.demarkus` state with the Claude Code plugin, so the two coexist on one machine: one soul, one token, one set of registries.

## What it does

- **Zero-config provisioning.** On session start it downloads the pinned demarkus binaries, generates a `0600` capability token, spawns a managed local `demarkus-server`, and registers the `demarkus-memory` MCP server in the pi-mcp-adapter config (`~/.config/mcp/mcp.json`).
- **Standing guidance.** Injects "recall first, record as you go" guidance once per session so the agent self-documents to the soul.
- **Publish tag-gate.** A tagless `mark_publish` is invisible to `mark_lookup` forever; the gate makes that loud at write time (`warn` by default, `block`/`ask` available).
- **Destination gate.** When a repo is bound to a specific soul, a write aimed at a different soul is denied (default `block`) so memory lands on the right store.
- **Recall / journal / promote nudges.** Discreet reminders at the moments they matter.
- **Slash commands.** `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-refresh`, `/promote`, `/promote-scan`, plus the on-demand `soul-memory` skill.

## Requirements

- [`pi-mcp-adapter`](https://www.npmjs.com/package/pi-mcp-adapter) — surfaces the demarkus MCP tools. Install with `pi install npm:pi-mcp-adapter`.
- `bash`, `curl`, `tar`, `node` on PATH (used by the bundled provisioning scripts).

## Install

First install the MCP adapter (provides the demarkus tools):

```bash
pi install npm:pi-mcp-adapter
```

This package lives in the demarkus monorepo under `plugins/pi-memory/`. pi's `git:` installer reads a repository's **root** `package.json`, so a monorepo subdirectory can't be git-installed directly — install it from a local checkout instead:

```bash
git clone https://github.com/latebit-io/demarkus
pi install ./demarkus/plugins/pi-memory     # add -l for project-local scope
```

If the plugin is published as its own repository, the one-line git form works too:

```bash
pi install git:github.com/latebit-io/demarkus-pi-memory
```

On the next session the soul is provisioned automatically. Run `/mcp` (or restart pi) once after the first session to connect the newly-registered `demarkus-memory` server. Use `/soul-status` to diagnose, `/soul-init` to reconfigure (adopt an existing server, switch modes). Remove with `pi remove ./demarkus/plugins/pi-memory`.

### Update

The command depends on how it was installed:

```bash
# installed from a local checkout
git -C demarkus pull && pi install ./demarkus/plugins/pi-memory

# installed via the git: one-liner
pi update git:github.com/latebit-io/demarkus-pi-memory
```

The standalone repo is mirrored from the monorepo on every push to `main`, so a `git:` update picks up a change once that mirror workflow has run. Restart pi (or run `/mcp`) after an update that changes the MCP wiring.

### Update check

On session start, after provisioning, the extension hands `demarkus-plugin update-check` its own `package.json` version along with this plugin's manifest URL and update command; the binary compares against the manifest on the monorepo's `main` branch and the extension notifies when a newer release exists. Notify-only: nothing installs itself, since pi owns the package lifecycle. The binary throttles to one check per 24h (stamp: `~/.demarkus/.update-check-demarkus-pi-memory`), the call is fire-and-forget so it cannot delay the session, and it stays silent when offline. Turn it off with `DEMARKUS_UPDATE_CHECK=0` or by writing `0` to `~/.demarkus/plugin.update-check`. It reads the monorepo (the source of truth), so a `git:`-installed copy can see the notice a workflow run before the mirror repo carries the new version.

## Architecture

- `src/*.ts` — native TypeScript: config/registry readers, the publish + destination gates, nudges, session-start guidance, the update check, and MCP wiring (loaded directly by pi via `tsx`, no build step).
- `scripts/*.sh` — the demarkus binary/server lifecycle, reused verbatim from the Claude Code plugin (single source of truth for provisioning); `provision.sh` is the per-session entrypoint and `mcp-config.mjs` registers remote-soul MCP servers.
- `commands/*.md` — slash-command prompt bodies, injected by the extension.
- `skills/soul-memory/` — the on-demand memory-routing skill.

### Behavior mapping (Claude Code → pi)

| Claude Code hook | pi |
|---|---|
| `SessionStart` (guidance) | `before_agent_start`, first turn |
| `UserPromptSubmit` recall-nudge | `before_agent_start`, on matching `event.prompt` |
| `PreToolUse` publish/dest gate | `tool_call` returning `{ block, reason }` |
| publish gate `warn` (PostToolUse) | `tool_call` allows + injects a reminder message |
| `PostToolUse` promote-nudge | `tool_call`, injected reminder on a fresh ADR publish |
| `Stop` journal-nudge | `session_shutdown` notify (pi can't force an extra turn) |

`ask` strictness maps to a block whose reason tells the agent to confirm with the user first (pi's `tool_call` has no native "ask").

## Syncing changes back to the standalone repo

This package is developed in the [demarkus monorepo](https://github.com/latebit-io/demarkus) under `plugins/pi-memory/` and mirrored to its own repo (`latebit-io/demarkus-pi-memory`) so it can be `pi install`ed via the `git:` one-liner. The monorepo is the source of truth.

After landing changes to `plugins/pi-memory/` in the monorepo, re-publish the standalone repo from a monorepo checkout:

```bash
# from the monorepo root, on the branch that has your committed changes
plugins/setup-pi-repos.sh            # --dry-run to preview
```

This is automated: the `.github/workflows/pi-plugin-mirror.yml` workflow runs on every push to `main` that touches `plugins/pi-memory/**` and mirrors it for you (it needs a `PI_MIRROR_TOKEN` repo secret with `contents:write` on the standalone repos). The `plugins/setup-pi-repos.sh` script is the manual fallback / bootstrap.

Either way it runs `git subtree split --prefix=plugins/pi-memory` to recompute the subdirectory's history and force-pushes it to the standalone repo's default branch — so the standalone repo always matches the monorepo subtree. It only reads **committed** history, so the change must be merged to `main` first. Users then pick up the change with `pi update git:github.com/latebit-io/demarkus-pi-memory`.
