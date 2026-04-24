---
name: memory
description: Use when the user wants to remember, save, recall, or query personal notes that should persist across Claude Code sessions. Routes through the demarkus-memory MCP tools against a local versioned markdown store organized by project.
---

# Memory

This skill routes "remember" / "recall" intents through the `demarkus-memory` MCP server (a local demarkus-server), which provides a versioned, link-graph-aware personal memory store. The store is organized by project: top-level `/index.md` is a project list, and each project lives at `/<slug>/` with a consistent internal structure.

## When to trigger

Trigger on phrases and intents like:

- "remember this", "save to memory", "note that", "jot down"
- "what do I know about X", "do I have notes on Y", "recall Z"
- "add this to my notes", "put this in my soul"
- "find anything about ...", "what have I saved about ..."

Do NOT trigger for ephemeral context ("remember that in this conversation we're using Python 3.12") — that belongs in the current session, not the persistent store.

## Determining the current project

Before reading or writing, resolve the project slug:

1. Use the basename of `CLAUDE_PROJECT_DIR` env var (via the Bash tool: `basename "$CLAUDE_PROJECT_DIR"`). Convert to a lowercase slug if needed (spaces → hyphens).
2. If `CLAUDE_PROJECT_DIR` is unset or the user is clearly asking about a different project, ask which project.
3. If the project doesn't yet exist in the soul (no `/<slug>/` subtree), create it on first write by publishing the relevant file(s) and adding a link to `/index.md`.

## Per-project structure

Each `/<project>/` subtree has a fixed layout:

- `/<project>/plan/tasks.md` — active work, priorities, what's in flight
- `/<project>/architecture.md` — system design, module boundaries, key decisions
- `/<project>/patterns.md` — code patterns, conventions, idioms
- `/<project>/roadmap.md` — done, next, not prioritized
- `/<project>/adr/<NNNN>-<slug>.md` — Architecture Decision Records (one per decision, zero-padded 4-digit sequence)
- `/<project>/journal/<YYYY-MM-DD>.md` — dated session notes, one file per day

## Tool routing

Read intents → start with `mark_fetch`:

1. Fetch `/index.md` to see the project list
2. Fetch `/<project>/` (directory listing) or a specific file under it
3. Use `mark_backlinks` or `mark_graph` to surface related documents across projects
4. If nothing relevant is found, say so honestly rather than fabricating

Write intents — route to the right file for the content type:

- **Fleeting observation / daily note** → append to today's `/<project>/journal/<YYYY-MM-DD>.md`. If the file does not exist yet, create it with `mark_publish` (`expected_version: 0`) and a header like `# <Project> journal — <YYYY-MM-DD>`, then append on subsequent calls. The `/soul-journal` command handles this automatically.
- **Architecture decision** → create a new ADR at `/<project>/adr/<NNNN>-<slug>.md` with `mark_publish` (`expected_version: 0`). Pick the next sequence number by listing the `adr/` directory. Standard ADR template: `# <NNNN>. <Title>`, `## Status`, `## Context`, `## Decision`, `## Consequences`.
- **Pattern / convention learned** → append to `/<project>/patterns.md`. Fetch first if updating an existing section; otherwise `mark_append` with `expected_version` unset.
- **Architecture change** → fetch `/<project>/architecture.md`, update the relevant section, publish with the correct `expected_version`.
- **Task / todo** → fetch and update `/<project>/plan/tasks.md`.
- **Roadmap item** → fetch and update `/<project>/roadmap.md`.
- **Cross-project or global note** → if it does not fit a project, ask the user where it belongs. Do not create ad-hoc top-level files.

Always reference what you saved by full path so the user can find it again.

## Creating a new project

When the user wants to start memory for a project that does not exist in `/index.md` yet:

1. Confirm the slug (basename of `CLAUDE_PROJECT_DIR`, lowercased, spaces → hyphens)
2. Create the first relevant file (e.g. today's journal entry, or `architecture.md` stub) with `mark_publish` `expected_version: 0`
3. Fetch `/index.md`, add a bullet `- [<Project Name>](/<slug>/)` under the project list, and publish with the correct `expected_version`

## Don't

- Don't fabricate memory. If `mark_fetch` returns `not-found`, the document does not exist — say so.
- Don't invent expected_versions. Use 0 for new documents; fetch first when updating.
- Don't bypass the MCP tools with shell commands — the soul is the source of truth.
- Don't create top-level files outside `/<project>/` subtrees except for `/index.md` itself.
- Don't save secrets, credentials, or anything the user has not explicitly authorized.
