---
layout: default
title: How It Works
permalink: /how-it-works/
---

# How knowledge flows

Demarkus is agent-driven memory. You do not file documents by hand. As the agent
works, it records what is worth keeping, and when a note is ready for other people
it promotes it to the shared catalog. Three stores, one flow.

```text
   you steer                 the agent files
   ─────────                 ───────────────
      │                            │
      ▼                            ▼
  a request  ──▶  [ agent ]  ──record──▶  SOUL          personal, local, drafts
                     │                     │
                     │ recall              │ /promote  (curate + human gate)
                     │ (both stores)       ▼
                     │             KNOWLEDGE SYSTEM      shared, authoritative
                     │                     │
                     └─────read────────────┤ /soul-refresh (authoritative wins)
                                           ▼
                                        LIBRARY          humans read the same store
```

The **soul** is your private draft tier. The **knowledge system** is the shared,
authoritative one. The [library](/library/) is a read surface over either, not a
third store.

## It is the agent, not you

The plugins do not add a "save note" button. They inject standing guidance and a
few hooks, so the agent reads and writes memory as a normal part of working. You
steer the session; the agent files as it goes. Nothing is published to the shared
catalog without a human gate.

Two plugins, each in its lane, composing in one install:

- **`demarkus-memory`** owns the personal soul: capture, recall, and the intent
  to promote.
- **`demarkus-knowledge`** owns the shared system: the consult-first bar and the
  curation cascade that gates what lands there.

They partition by server scope, so a write never lands in the wrong store.

## Into personal memory

At the start of substantive work the agent **recalls** before answering from the
current context alone: `mark_lookup` for the subject, `mark_fetch` the project
hub, `mark_backlinks` for related work. If nothing comes back it says so rather
than inventing memory.

As the work produces something durable, the agent **records** it to the right
place, without being asked:

| What | Where |
|---|---|
| A decision | an ADR at `/<project>/adr/<NNNN>-<title>.md` |
| A bug lesson or gotcha | `/<project>/debugging.md` |
| A pattern or convention | `/<project>/patterns.md` |
| End-of-session progress | today's `/<project>/journal/<YYYY-MM-DD>.md` |

Memory is organized per project under `/<project>/`. Three hooks keep the catalog
honest: a publish without tags is caught at write time (an untagged doc is
invisible to `mark_lookup`), an end-of-session nudge asks for a journal entry when
files changed but nothing was recorded, and a recall nudge fires on a "did we
decide X" question. They are reminders, not gates.

Links between documents accumulate into a persistent **knowledge graph**: each
`mark_graph` crawl merges into a graph stored across sessions, and
`mark_backlinks` answers "what links here?" without re-crawling. Recall is graph
traversal, not just search; an ADR pulls in the debugging note that motivated it
and the pattern it produced. At the system tier the [indexing
agent](/ecosystem/#indexing-agent) aggregates the graph across worlds and
publishes it to the hub, so a cold agent seeds its graph on the first call.

This is the whole loop for one person. The soul is fast, private, and
yours: on your own machine, or on a [remote host](/scenarios/agent-memory/)
you join from every device.

## Up to the knowledge system

Most soul content should stay personal. A debugging note about your local setup,
a half-formed idea, a session journal: none of that belongs in the shared catalog.
Promotion is the deliberate step that lifts the small fraction that does.

`/promote <soul-path>` runs the **curation cascade** (the `knowledge-promote`
skill on the knowledge side):

1. **Triage.** Durable? Broadly useful? Not already in the catalog? Most soul
   content correctly fails here and stays personal.
2. **Distill.** Rewrite for a shared audience, stripping personal and local
   framing and any secrets or PII. This strips them, it does not summarize around
   them.
3. **Dedup.** `mark_lookup` the destination for an existing document on the
   subject; an update re-enters as a gated change, not a duplicate.
4. **Tag.** Apply the destination's taxonomy, honoring any required tag axes and
   OKF fields its policy declares.
5. **Route.** Pick a writable world via `mark_worlds` and each world's `world.md`.
6. **Gate.** A human approves, capped by the world's autonomy ceiling. Nothing
   auto-publishes to a shared store.
7. **Publish with provenance,** then **back-stamp** the soul doc so it is not
   re-promoted and can be refreshed later.

`/promote-scan` sweeps the soul for candidates worth lifting, and publishing a new
ADR nudges promotion. The trigger is always explicit; the human always gates.

## Staying coherent

Once promoted, the authoritative copy lives in the knowledge system and may move
on. `/soul-refresh` is the downward leg: it finds promoted docs (they carry a
`promoted:` marker and tag), checks each against the live copy, and refreshes the
stale ones. Reconciliation is **directional**: knowledge is the base of truth, the
soul refreshes from it, and any local edits go back up through `/promote`'s gate,
never as a silent two-way merge.

So the edge between the two stores is a loop with a clear direction: draft in the
soul, promote up through the gate, refresh back down as the shared copy evolves.

## Using it day to day

A typical session:

1. **Orient.** `/soul-context` restores where you left off; for a shared subject
   the agent consults the knowledge system first (`/knowledge` to navigate it).
2. **Work.** The agent recalls as questions come up and records decisions,
   lessons, and patterns as they land.
3. **Close out.** `/soul-journal` captures progress if the Stop nudge has not
   already prompted it.
4. **Promote when ready.** `/promote` a note that others should rely on, or
   `/promote-scan` to sweep for candidates. The cascade gates it into the catalog.
5. **Refresh.** `/soul-refresh` pulls promoted docs back down as their
   authoritative copies change.

For anything shared or organizational, the knowledge system is the first place to
look and the source of truth; the soul is your scratch space and personal backstop.
Humans browse the same catalog in the [library](/library/); agents read it over
MCP. Same store, same versions.

## Where each tier fits

- [Agent memory](/scenarios/agent-memory/): the personal soul, the capture loop, the plugin setup.
- [Organizational knowledge system](/scenarios/knowledge-system/): the shared, broker-fronted catalog and how teams join.
- [The library](/library/): the human reading room over either.
