---
layout: default
title: How It Works
permalink: /how-it-works/
---

# How knowledge flows

Demarkus is agent-driven memory. As the agent works, it records what is worth
keeping. When a note is ready for other people, it promotes it to the shared
catalog. Two stores, one flow.

```text
                 you steer
                     │
                     ▼
                  [ agent ] ──record──▶ MEMORY
                     │                 personal, drafts
                     │                     │
                     │                 /promote
                     │                 human gate
                     │                     ▼
                     └──consult────▶ KNOWLEDGE SYSTEM
                                     shared, authoritative
                                           │
                                        LIBRARY
                                     humans read here
```

Your **memory**, also called a soul, is the private draft tier. The **knowledge
system** is the shared, authoritative one. The [library](/library/) reads the same
documents; it is not another store.

## The agent files as it goes

Two plugins compose in one agent:

- **`demarkus-memory`** captures and recalls personal memory, locally or remotely.
- **`demarkus-knowledge`** consults the shared system and curates what lands there.

The agent recalls before substantive work, records decisions and lessons as they
land, and journals progress. Hooks reinforce recall, metadata, and destination
rules; shared publishing goes through a human gate.

| What | Where |
|---|---|
| A decision | `/<project>/adr/<NNNN>-<title>.md` |
| A bug lesson | `/<project>/debugging.md` |
| A pattern | `/<project>/patterns.md` |
| Session progress | `/<project>/journal/<YYYY-MM-DD>.md` |

A single-project store can use the root. The store's template declares the layout.

## Recall with evidence

`mark_lookup` finds documents by tags and title; `match=body` also finds matching
sections. A `budget` can include focused passages and related documents. Fetch
the returned `#anchor` when you need more, rather than loading whole files.

Links and typed relations form the graph. `mark_graph` follows outbound links;
`mark_backlinks` queries the revision-aware graph with bounded source checks.
In a knowledge system, the server derives the cross-world graph in process.

For a shared subject, `mark_lookup_all` searches every readable world in one call.
Failures and partial results stay visible.

## Promote what others should rely on

Most memory stays personal. `/promote <path>` lifts the useful fraction through
the `knowledge-promote` skill:

1. **Triage and distill.** Keep durable knowledge; remove personal framing and secrets.
2. **Route and check.** Pick a writable world; check existing content, policy, and style.
3. **Review.** Show the dry run and get approval.
4. **Publish.** Apply with provenance and mark the source as promoted.

`/promote-scan` finds candidates. `/soul-refresh` reconciles promoted notes with
their shared copies through review. The knowledge system stays authoritative;
shared knowledge is linked or fetched fresh.

## Day to day

- `/soul-context`: restore where you left off.
- `/knowledge`: navigate joined systems.
- `/soul-journal`: record progress.
- `/soul-doctor`: audit memory hygiene.
- `/soul-curate`: review and reshape a document.
- `/promote`: publish a ready note through the shared gate.

## Where each tier fits

- [Agent memory](/scenarios/agent-memory/): personal capture, recall, and setup.
- [Knowledge system](/scenarios/knowledge-system/): the shared catalog and team login.
- [The library](/library/): the human reading room.
