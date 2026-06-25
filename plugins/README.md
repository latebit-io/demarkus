# demarkus coding-agent plugins

Same demarkus behavior — local **memory** (a "soul") and organizational **knowledge** systems — across multiple coding agents, with **one shared decision engine** and a thin per-harness adapter on top.

## The architecture: one binary, thin adapters

All harness-agnostic logic and orchestration live in a single Go binary, **`demarkus-plugin`** (in [`tools/demarkus-plugin`](../tools/demarkus-plugin); shipped in the `tools/` release, installed to `~/.demarkus/bin` by each plugin's `scripts/bootstrap.sh`):

- **`gate`** — the write-time publish tag-gate + destination gate + knowledge axes/fields gate.
- **`nudge`** — recall / promote / session-end reminders.
- **`guidance`** — session-start standing context (+ one-time offers, health warnings).
- **`registry`** — locked, atomic reads/mutations of `~/.demarkus` state (souls, knowledge systems, policy mirrors, promote targets, MCP config).
- **`provision`** / **`mcp-serve`** — managed-server lifecycle and the local-soul MCP launcher.

Each **plugin is a thin adapter**: a manifest (its harness's hook/event wiring), one-line hook scripts that pipe the harness's hook JSON to the binary with a `--format` for that harness, and the shared prompt content (commands + standing guidance). A fix in the binary fixes every harness at once; **a new harness reimplements nothing** — it only re-expresses the wiring.

```
harness hook/event ──▶ thin adapter ──▶ demarkus-plugin <cmd> --format <harness> ──▶ decision ──▶ adapter translates back
                                              (all logic here, shared)
```

## Harness matrix

| Harness | Memory | Knowledge | Wiring | Gate output format |
|---|---|---|---|---|
| Claude Code | [`claude-code`](claude-code/) | [`claude-code-knowledge`](claude-code-knowledge/) | `plugin.json` hooks + marketplace | `claude-pre` / `claude-post` / `claude` |
| pi | [`pi-memory`](pi-memory/) | [`pi-knowledge`](pi-knowledge/) | TS extension (`src/`) + `package.json` | `json` (native) |
| Codex | [`codex-memory`](codex-memory/) | [`codex-knowledge`](codex-knowledge/) | `config.toml` hooks + `mcp_servers` | `codex-pre` / `codex-post` / `codex` |

The three harnesses' hook contracts converge: Claude Code and Codex share the same `hookSpecificOutput` / `{decision,reason}` schema and event names (`SessionStart`, `PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `Stop`), so the binary's `claude*` and `codex*` formats differ only where the harnesses do (Codex's `PreToolUse` has no `ask`, so `codex-pre` folds `ask` into `deny` with a "confirm first" reason). pi consumes the native `json` decision directly in TypeScript.

## Install

- **Claude Code** — add the marketplace (`.claude-plugin/marketplace.json`) and enable `demarkus-memory` / `demarkus-knowledge`.
- **pi** — `pi install git:github.com/latebit-io/demarkus-pi-memory` (and `…-pi-knowledge`); the standalone repos are mirrored from this monorepo. See each plugin's README.
- **Codex** — run `plugins/codex-memory/install.sh` (and/or `codex-knowledge/install.sh`) from a checkout; it bootstraps the binary, writes a managed block into `~/.codex/config.toml`, and installs the command procedures as skills under `~/.agents/skills/` (invoke with `/skills` or `$name`).

All three share the same `~/.demarkus` state on a machine — a soul joined or policy set under one harness is visible to all.

## Adding a new harness

The Codex plugins are the worked proof that a new harness is adapter-only. To add one:

1. **Wire the harness's lifecycle** to the binary's subcommands: session-start → `provision` (memory only) + `guidance`; pre-write → `gate`; post-write → `gate` (warn reminder) + `nudge` (promote); prompt → `nudge` (recall); session-end → `nudge` (session-end).
2. **Pick or add an output format.** If the harness's hook schema matches Claude/Codex, reuse `claude*`/`codex*`. Otherwise add a small `--format` case in [`tools/demarkus-plugin/main.go`](../tools/demarkus-plugin/main.go) (`emitDecision` for the gate; the `cmdNudge`/`cmdGuidance` branches for the rest) — this is the only Go change a new harness should need.
3. **Wire MCP**: launch the local soul via `demarkus-plugin mcp-serve`; add knowledge brokers per the harness's MCP mechanism.
4. **Reuse the prompt content** (`context/session-guidance.md` + the command markdown), adapting only the harness-specific MCP-registration steps in the join prompts.

## Versioning & pins

The managed-binary pins (server / client / tools) are the Go consts in [`provision.go`](../tools/demarkus-plugin/internal/provision/provision.go); `toolsVersion` must match the `TOOLS_VERSION` in every plugin's `bootstrap.sh`. The [`plugin-pin-bump.yml`](../.github/workflows/plugin-pin-bump.yml) workflow keeps them current and patch-bumps the affected plugin versions.
