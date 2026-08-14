---
name: soul-memory
description: Use when the user wants to remember, save, recall, or query personal notes that should persist across pi sessions. Routes through the demarkus-memory MCP tools against a local versioned markdown store organized by project.
---

# Soul Memory

This skill routes "remember" / "recall" intents through the `demarkus-memory` MCP server (a local demarkus-server, surfaced by pi-mcp-adapter — tools are typically named `demarkus_memory_mark_*`), which provides a versioned, link-graph-aware personal memory store. The store is organized by project: top-level `/index.md` is a project list, and each project lives at `/<slug>/` with a consistent internal structure.

## When to trigger

Trigger on phrases and intents like:

- "remember this", "save to memory", "note that", "jot down"
- "what do I know about X", "do I have notes on Y", "recall Z"
- "add this to my notes", "put this in my soul"
- "find anything about ...", "what have I saved about ..."

Do NOT trigger for ephemeral context ("remember that in this conversation we're using Python 3.12") — that belongs in the current session, not the persistent store.

## Determining the current project

Before reading or writing, resolve the project slug:

1. Use the basename of the project directory (the session cwd). Convert to a lowercase slug if needed (spaces → hyphens).
2. If the user is clearly asking about a different project, ask which project.
3. If the project doesn't yet exist in the soul (no `/<slug>/` subtree), create it on first write by publishing the relevant file(s) and adding a link to `/index.md`.

## Per-project structure

This skill carries the canonical layout. A `/project-template.md` published at the soul root overrides it — fetch that and follow it when it exists. Only `not-found` means no override; any other fetch failure (transport, auth, server error) is a real error — surface it and stop instead of silently falling back to this layout. Each `/<project>/` subtree uses:

- `/<project>/index.md` — the project hub; links to every doc below and anchors discovery
- `/<project>/architecture.md` — system design, module boundaries, key decisions
- `/<project>/patterns.md` — code patterns, conventions, idioms
- `/<project>/guidelines.md` — hard rules for code quality; read before writing code
- `/<project>/debugging.md` — lessons from bugs and investigations
- `/<project>/roadmap.md` — done, next, deliberately not prioritized
- `/<project>/debt.md` — technical debt and improvement opportunities
- `/<project>/thoughts.md` — open questions, reflections, ideas
- `/<project>/adr/<NNNN>-<slug>.md` — Architecture Decision Records (one per decision, zero-padded 4-digit sequence)
- `/<project>/plans/<name>.md` — plan documents (lifecycle carried in the text)
- `/<project>/journal/<YYYY-MM-DD>.md` — dated session notes, one file per day

Map the layout to the OKF `type` on publish: `architecture.md` → `Architecture`, `adr/*` → `Decision`, `plans/*` and `roadmap.md` → `Plan`, `journal/*` → `Journal`, `patterns.md`/`guidelines.md`/`debugging.md` → `Guide`, standalone reference docs → `Reference`. `debt.md` and `thoughts.md` take the server default (`Document`); hubs (`index.md`) stay untyped.

## Tool routing

Read intents → start with `mark_lookup` (the catalog), then `mark_fetch`:

1. `mark_lookup` with `url=/<project>/` (or `/` to span every project) and a subject `query` — returns an importance-ranked markdown table of matching docs (path, importance, title, tags), not bodies. This is a catalog lookup, not full-text search: it finds only what was tagged or titled. Narrow with the optional `filter` arg (`tag=`, `modified-after=`, `modified-before=`) and cap with `limit`.
2. `mark_fetch` the rows worth reading. Also `mark_fetch /index.md` / `/<project>/index.md` directly — lookup won't surface untagged docs, so the index hub still anchors discovery.
3. Use `mark_backlinks` or `mark_graph` to surface related documents across projects
4. If nothing relevant is found, say so honestly rather than fabricating

Write intents — route to the right file for the content type:

- **Fleeting observation / daily note** → append to today's `/<project>/journal/<YYYY-MM-DD>.md`. If the file does not exist yet, create it with `mark_publish` (`expected_version: 0`) and a header like `# <Project> journal — <YYYY-MM-DD>`, then append on subsequent calls. The `/soul-journal` command handles this automatically.
- **Architecture decision** → create a new ADR at `/<project>/adr/<NNNN>-<slug>.md` with `mark_publish` (`expected_version: 0`). Pick the next sequence number by listing the `adr/` directory. Standard ADR template: `# <NNNN>. <Title>`, `## Status`, `## Context`, `## Decision`, `## Consequences`.
- **Pattern / convention learned** → append to `/<project>/patterns.md`. Fetch first if updating an existing section; otherwise `mark_append` with `expected_version` unset.
- **Hard rule for code quality** → `/<project>/guidelines.md`.
- **Lesson from a bug / a gotcha** → `/<project>/debugging.md` (high recall value — capture the non-obvious why).
- **Architecture change** → fetch `/<project>/architecture.md`, update the relevant section, publish with the correct `expected_version`.
- **What's next / done / not prioritized** → `/<project>/roadmap.md`.
- **Technical debt** → `/<project>/debt.md`.
- **Plan document** → `/<project>/plans/<name>.md` (carry active / completed / archived in the text).
- **Open question / idea, not yet decided** → `/<project>/thoughts.md`.
- **Cross-project or global note** → if it does not fit a project, ask the user where it belongs. Do not create ad-hoc top-level files (the soul root holds only `/index.md` and an optional `/project-template.md` override).

**On every `mark_publish`, set `metadata`:** `tags` (comma-separated subjects — the primary match target for `mark_lookup`) and, sparingly, `importance` (0–1, default 0.5; reserve high values for genuinely central docs like index hubs and architecture). An untagged document can only be found by words in its title, so tagging on write is what makes later recall work. The server does not infer either field — you choose them. Reserved keys are rejected; any other metadata key is stored opaquely and reachable through lookup's `filter` axis.

**All metadata goes in the `metadata` object, never in the body.** The recognized keys are `title`, `tags`, `importance`, and the OKF `type` (the document's kind). Never hand-write a YAML frontmatter block at the top of a document body — a `---` … `---` fence, or `name:` / `description:` / `type:` keys. demarkus prepends its own version envelope and carries metadata out of band, so a body that opens with `---` is stored as literal content: it renders as garbled headings in a viewer and is invisible to `mark_lookup`. Express the intent through the real channel — the document's name is its `# H1` heading (or `metadata.title`), its **kind is the OKF `type` field** (a `type` key in the `metadata` object, e.g. `metadata: {"type": "Reference"}`), and a one-line summary is the first sentence under the H1.

`mark_append` preserves a doc's metadata: the server carries the current version's `tags`/`importance`/`title`/`type` onto the appended version (`retention` is never inherited), so appending never costs a doc its catalog entry. It cannot add to them, so when an append introduces a materially new subject, re-publish the doc (full body, correct `expected_version`) with its complete current metadata plus the extended `tags`. `mark_publish` replaces the metadata map, so publishing tags alone discards `importance`, `title`, `type`, and any opaque keys.

Always reference what you saved by full path so the user can find it again.

## Creating a new project

When the user wants to start memory for a project that does not exist in `/index.md` yet (the per-project structure above is the layout; a published `/project-template.md` override wins):

1. Confirm the slug (basename of the project cwd, lowercased, spaces → hyphens)
2. Create the project hub `/<slug>/index.md` with `mark_publish` `expected_version: 0` — a short page that will link to the project's docs as they appear. Don't pre-create empty stubs for every template file; add docs when there's something real to put in them.
3. Create the first content doc (today's journal entry, or an `architecture.md` stub) and link it from `/<slug>/index.md`.
4. Fetch `/index.md`, add a bullet `- [<Project Name>](/<slug>/)` under the project list, and publish with the correct `expected_version`

## Don't

- Don't fabricate memory. If `mark_fetch` returns `not-found`, the document does not exist — say so.
- Don't invent expected_versions. Use 0 for new documents; fetch first when updating.
- Don't bypass the MCP tools with shell commands — the soul is the source of truth.
- Don't create top-level files outside `/<project>/` subtrees except for `/index.md` and a `/project-template.md` layout override.
- Don't save secrets, credentials, or anything the user has not explicitly authorized.
