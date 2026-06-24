# demarkus-pi-knowledge

Join an organizational [demarkus](https://github.com/latebit-io/demarkus) **knowledge system** — a broker-fronted, shared, versioned markdown catalog — from the [pi](https://pi.dev) coding agent. The pi port of the Claude Code `demarkus-knowledge` plugin. It owns no server and no binaries: a knowledge system is reached over HTTPS via [`pi-mcp-adapter`](https://www.npmjs.com/package/pi-mcp-adapter)'s OAuth. It shares `~/.demarkus` state (the registry + per-system policy mirrors) with the rest of the demarkus tooling, so it composes with `demarkus-pi-memory`.

## What it does

- **Join over OAuth.** `/knowledge-join <broker-url>` validates the broker (RFC 9728 metadata), derives a slug, and registers it as an HTTP MCP server (`{url, auth: "oauth"}`) in `~/.config/mcp/mcp.json`. pi-mcp-adapter runs the OAuth flow on first use — no token stored locally.
- **Consult-first guidance.** Names the joined systems and injects "check the shared catalog first, navigate via the `root` hub, record durable shared knowledge there" guidance, once per session.
- **Publish tag-gate.** Enforces `tags` + `importance` + the system's policy-declared **required tag axes** (`axis:value`) and **required OKF fields** (e.g. `type`) on writes to a joined system — `warn` by default, `block`/`ask` per the mirrored policy.
- **Recall nudge.** On recall-shaped prompts about shared/org knowledge, reminds the agent to look in the system first.
- **Slash commands.** `/knowledge`, `/knowledge-join`, `/knowledge-doctor`, plus the `knowledge-promote` cascade skill.

## Requirements

- [`pi-mcp-adapter`](https://www.npmjs.com/package/pi-mcp-adapter) (HTTP + OAuth transport). Install with `pi install npm:pi-mcp-adapter`.
- `bash`, `curl`, `node` on PATH (used by the bundled join/policy scripts).

## Install

First install the MCP adapter (provides the HTTP/OAuth transport):

```bash
pi install npm:pi-mcp-adapter
```

This package lives in the demarkus monorepo under `plugins/pi-knowledge/`. pi's `git:` installer reads a repository's **root** `package.json`, so a monorepo subdirectory can't be git-installed directly — install it from a local checkout instead:

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

Run `/mcp` to connect (and `/mcp-auth <slug>` if prompted) to complete OAuth. `/knowledge` lists joined systems and orients you on each `root` hub; `/knowledge-doctor` audits catalog hygiene. Remove with `pi remove ./demarkus/plugins/pi-knowledge`.

## Architecture

- `src/*.ts` — native TypeScript: registry + per-slug policy readers, the publish gate (tags / importance / required axes / required fields), the recall nudge, and session-start guidance (loaded directly by pi via `tsx`).
- `scripts/*.sh` — broker URL validation (`knowledge-join.sh`), registry write (`register-knowledge.sh`), and deterministic policy mirroring (`mirror-policy.sh`), reused verbatim from the Claude Code plugin; `mcp-config.mjs` registers the broker as an HTTP/OAuth MCP server.
- `commands/*.md` — slash-command prompt bodies, injected by the extension.
- `skills/knowledge-promote/` — the soul → knowledge promotion cascade.

### Behavior mapping (Claude Code → pi)

| Claude Code hook | pi |
|---|---|
| `SessionStart` (guidance / one-time join hint) | `before_agent_start`, first turn |
| `UserPromptSubmit` recall-nudge | `before_agent_start`, on matching `event.prompt` |
| `PreToolUse` publish gate | `tool_call` returning `{ block, reason }` |
| publish gate `warn` (PostToolUse) | `tool_call` allows + injects a reminder message |
| `claude mcp add --transport http` | `mcp-config.mjs add-http` → `{url, auth: "oauth"}` |

`ask` strictness maps to a block whose reason tells the agent to confirm with the user first (pi's `tool_call` has no native "ask").

## Syncing changes back to the standalone repo

This package is developed in the [demarkus monorepo](https://github.com/latebit-io/demarkus) under `plugins/pi-knowledge/` and mirrored to its own repo (`latebit-io/demarkus-pi-knowledge`) so it can be `pi install`ed via the `git:` one-liner. The monorepo is the source of truth.

After landing changes to `plugins/pi-knowledge/` in the monorepo, re-publish the standalone repo from a monorepo checkout:

```bash
# from the monorepo root, on the branch that has your committed changes
plugins/setup-pi-repos.sh            # --dry-run to preview
```

This is automated: the `.github/workflows/pi-plugin-mirror.yml` workflow runs on every push to `main` that touches `plugins/pi-knowledge/**` and mirrors it for you (it needs a `PI_MIRROR_TOKEN` repo secret with `contents:write` on the standalone repos). The `plugins/setup-pi-repos.sh` script is the manual fallback / bootstrap.

Either way it runs `git subtree split --prefix=plugins/pi-knowledge` to recompute the subdirectory's history and force-pushes it to the standalone repo's default branch — so the standalone repo always matches the monorepo subtree. It only reads **committed** history, so the change must be merged to `main` first. Users then pick up the change with `pi update git:github.com/latebit-io/demarkus-pi-knowledge`.
