# demarkus-memory

Persistent, versioned project memory for Claude Code, served by a local demarkus server: the **soul**.

This is the personal counterpart to [`demarkus-knowledge`](../claude-code-knowledge), which connects Claude Code to a shared organizational knowledge system. The two are independent plugins and compose cleanly: draft and think in the soul, promote to the knowledge system when the material is ready for others.

## Install

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

Zero-config: the plugin spawns a local `demarkus-server`, generates a capability token, and wires the `mark_*` MCP tools (fetch, list, lookup, publish, append, archive, graph, backlinks, and more).

## What it does

- **`/soul`**: show the local soul index.
- **`/soul-init`**: detect and configure the soul (reuse existing or install new).
- **`/soul-join`**: join a remote demarkus soul, store its token in a 0600 file, and bind it to the project.
- **`/soul-default`**: set the project's default demarkus write target.
- **`/soul-context`**: restore session context from the project's soul (recent journal, active work).
- **`/soul-journal`**: append an entry to today's journal.
- **`/soul-status`**: show connection state and verify health.
- **`/soul-doctor`**: read-only hygiene audit (orphans, broken links, untagged docs, stale index entries, ADR gaps).
- **`/promote`**: lift a soul document to a shared knowledge system (curate, route, gate, publish, back-stamp).
- **`/promote-scan`**: sweep the soul for promotion candidates.
- **`/soul-refresh`**: refresh promoted documents from the knowledge system, the downward leg of the coherence edge.
- **`soul-memory` skill**: routing rules for recall and recording (which file gets decisions, lessons, patterns, journal entries).

## Hooks

- **SessionStart**: standing guidance so sessions consult the soul first and record as they go.
- **Publish tag-gate (Pre/PostToolUse)**: enforces `tags` and sane `importance` on `mark_publish` so documents stay findable via `mark_lookup`.
- **Journal nudge (Stop)**: end-of-session reminder to journal when files changed but nothing was recorded.
- **Recall and promote nudges (UserPromptSubmit)**: reminders to check the soul on recall-shaped questions and to promote ready material.

## Local state

The plugin bootstraps the shared `demarkus-plugin` helper and keeps its state under `~/.demarkus/`: the managed server, its token, `plugin-memory.conf`, and per-project soul bindings in `project-souls`.

## Development

Runtime prompt files under `commands/`, `context/`, and `skills/` are generated from `plugins/prompt-source/`. Edit the canonical templates and run `cd tools && go run ./plugin-prompts write`; generated files are distribution artifacts.
