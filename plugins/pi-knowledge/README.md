# demarkus-pi-knowledge

Join an organizational [demarkus](https://github.com/latebit-io/demarkus) **knowledge system** from the [Pi](https://pi.dev) coding agent. It runs no local server; the small shared `demarkus-plugin` helper provides gates, guidance, and registry behavior while the MCP adapter owns broker OAuth.

## What it does

- **Join over OAuth.** `/knowledge-join <broker-url>` validates the broker (RFC 9728 metadata), derives a slug, and records it as an HTTP MCP server (`{url, auth: "oauth"}`) in `~/.config/mcp/mcp.json`. Pi's built-in MCP support connects it and runs the OAuth sign-in (with `pi-mcp-adapter` installed, the adapter does); the plugin stores no token.
- **Consult-first guidance.** Names the joined systems and injects "check the shared catalog first, navigate via the `root` hub, record durable shared knowledge there" guidance, once per session. Catalog recall prefers the broker-wide `mark_lookup_all`, one lookup across every readable world, with a per-world fallback for older brokers and plain endpoints.
- **Publish tag-gate.** Enforces `tags` + `importance` on writes to a joined system before the call leaves the session; the server enforces the system's policy (required tag axes, allowed values, required fields, strictness) on every write.
- **Recall nudge.** On recall-shaped prompts about shared/org knowledge, reminds the agent to look in the system first.
- **Slash commands.** `/knowledge`, `/knowledge-join`, `/knowledge-doctor`, plus the `knowledge-promote` cascade skill.

## Requirements

- pi 0.99 or later, whose built-in MCP support provides the HTTP and OAuth transport. On an older pi, install [`pi-mcp-adapter`](https://www.npmjs.com/package/pi-mcp-adapter) first (`pi install npm:pi-mcp-adapter`). Where the adapter is installed, on any version, the plugin leaves the connection to it. A server of the same name in pi's own `mcp.json` overrides the plugin's; when it points somewhere else, the plugin warns and registers nothing under that name.
- `bash`, `curl`, `node` on PATH (used by `scripts/bootstrap.sh`).

## Install

This package lives in the demarkus monorepo under `plugins/pi-knowledge/`. pi's `git:` installer reads a repository's **root** `package.json`, so a monorepo subdirectory can't be git-installed directly; install it from a local checkout instead:

```bash
git clone https://github.com/latebit-io/demarkus
pi install ./demarkus/plugins/pi-knowledge   # add -l for project-local scope
```

If the plugin is published as its own repository, the one-line git form works too:

```bash
pi install git:github.com/latebit-io/demarkus-pi-knowledge
```

Then join a system and connect it:

```bash
/knowledge-join https://mcp.broker.your-org.com
```

The system connects on your next prompt; sign in with `/mcp login <slug>`. With `pi-mcp-adapter`, run `/mcp` to connect (and `/mcp-auth <slug>` if prompted). `/knowledge` lists joined systems and orients you on each `root` hub; `/knowledge-doctor` audits catalog hygiene. Remove with `pi remove ./demarkus/plugins/pi-knowledge`.

## Architecture

Pi adapter CI typechecks this adapter; the built-in MCP wiring (`src/native.ts`, identical to pi-memory's) is tested in pi-memory. Generated prompt artifacts and shared helper behavior are checked separately.

- `src/*.ts`: the Pi adapter that maps events and MCP registration onto the shared helper's gate, nudge, and guidance decisions.
- `scripts/bootstrap.sh`: installs the shared helper, which validates the broker URL, writes the registry and registers the broker as an HTTP/OAuth MCP server.
- `commands/*.md`, `context/`, and `skills/`: generated distribution artifacts. Edit the monorepo's `plugins/prompt-source/`, then run `cd tools && go run ./plugin-prompts write`.

### Behavior mapping (Claude Code → pi)

| Claude Code hook | pi |
|---|---|
| `SessionStart` (guidance / one-time join hint) | `before_agent_start`, first turn |
| `UserPromptSubmit` recall-nudge | `before_agent_start`, on matching `event.prompt` |
| `PreToolUse` publish gate | `tool_call` returning `{ block, reason }` |
| publish gate `warn` (PostToolUse) | `tool_call` allows + injects a reminder message |
| `claude mcp add --transport http` | `mcp-config.mjs add-http` → `{url, auth: "oauth"}` |

## Syncing changes back to the standalone repo

This package is developed in the [demarkus monorepo](https://github.com/latebit-io/demarkus) under `plugins/pi-knowledge/` and mirrored to its own repo (`latebit-io/demarkus-pi-knowledge`) so it can be `pi install`ed via the `git:` one-liner. The monorepo is the source of truth.

After landing changes to `plugins/pi-knowledge/` in the monorepo, re-publish the standalone repo from a monorepo checkout:

```bash
# from the monorepo root, on the branch that has your committed changes
plugins/setup-pi-repos.sh            # --dry-run to preview
```

This is automated: the `.github/workflows/pi-plugin-mirror.yml` workflow runs on every push to `main` that touches `plugins/pi-knowledge/**` and mirrors it for you (it needs a `PI_MIRROR_TOKEN` repo secret with `contents:write` on the standalone repos). The `plugins/setup-pi-repos.sh` script is the manual fallback / bootstrap.

Either way it runs `git subtree split --prefix=plugins/pi-knowledge` to recompute the subdirectory's history and force-pushes it to the standalone repo's default branch, so the standalone repo always matches the monorepo subtree. It only reads **committed** history, so the change must be merged to `main` first. Users then pick up the change with `pi update git:github.com/latebit-io/demarkus-pi-knowledge`.
