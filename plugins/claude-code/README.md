# demarkus-memory

Persistent, versioned project memory for Claude Code, served by a local demarkus server: your **soul**, a memory store of your own.

This is the personal counterpart to [`demarkus-knowledge`](../claude-code-knowledge), which connects Claude Code to a shared organizational knowledge system. The two are independent plugins and compose cleanly: draft and think in your soul, promote to the knowledge system when the material is ready for others.

## Install

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

Zero-config: the plugin spawns a local `demarkus-server`, generates a capability token, and wires the `mark_*` MCP tools (fetch, list, lookup, publish, append, archive, graph, backlinks, and more).

## What it does

- **`/soul`**: show your soul's index.
- **`/soul-init`**: detect and configure your soul (reuse existing or install new).
- **`/soul-join`**: join a remote soul, store its token in a 0600 file, and bind it to the project.
- **`/soul-default`**: set the project's default demarkus write target.
- **`/soul-context`**: restore session context from the project's soul (recent journal, active work).
- **`/soul-journal`**: append an entry to today's journal.
- **`/soul-status`**: show connection state and verify health.
- **`/soul-doctor`**: read-only hygiene audit (orphans, broken links, untagged docs, stale index entries, hub and document shape, ADR gaps), one `mark_doctor` call over the store.
- **`/soul-curate`**: bring one document into shape (summary, stable headings, hub rules, a split into hub plus topic files when it outgrows one fetch): `mark_curate` reports and proposes, a human gate, `mark_curate` applies.
- **`/soul-archive`**: archive one document or every live document under a directory (history kept, hidden from lists and lookup) and drop the hub bullets that linked it: one `mark_archive` call after a human gate.
- **`/promote`**: lift a soul document to a shared knowledge system: `mark_promote` stages it, the knowledge plugin's skill distills and publishes through `mark_promote` on the destination, `mark_promote` back-stamps.
- **`/promote-scan`**: sweep the soul for promotion candidates (`mark_doctor` with `promotion`).
- **`/soul-refresh`**: refresh promoted documents from the knowledge system, the downward leg of the coherence edge: `mark_promote` compares the soul copy with the live body and republishes it after the gate.
- **`remember` skill**: routing rules for recall and recording (which file gets decisions, lessons, patterns, journal entries).

## Hooks

- **SessionStart**: standing guidance so sessions consult the soul first and record as they go.
- **Publish tag-gate (Pre/PostToolUse)**: enforces `tags` and sane `importance` on `mark_publish` so documents stay findable via `mark_lookup`.
- **Journal nudge (Stop)**: end-of-session reminder to journal when files changed but nothing was recorded.
- **Recall and promote nudges (UserPromptSubmit)**: reminders to check the soul on recall-shaped questions and to promote ready material.

## Local state

The plugin bootstraps the shared `demarkus-plugin` helper and keeps its state under `~/.demarkus/`: the managed server, its token, `plugin-memory.conf`, and per-project soul bindings in `project-souls`.

## Development

Runtime prompt files under `commands/`, `context/`, and `skills/` are generated from `plugins/prompt-source/`. Edit the canonical templates and run `cd tools && go run ./plugin-prompts write`; generated files are distribution artifacts.
