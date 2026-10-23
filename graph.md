---
layout: default
title: The Knowledge Graph
permalink: /graph/
---

# The knowledge graph

Every link between documents is an edge. Memory and knowledge here are a graph
the agent walks, not a pile of files it greps.

<div class="memory-demo memory-demo--wide">
  <img src="/demarkus-soul-graph.svg" alt="Knowledge graph of soul.demarkus.io: 261 documents and 840 edges, clustered by prefix, with typed relations highlighted" />
  <p class="memory-demo-caption">This project's memory, crawled and drawn by an agent in one mark_graph call</p>
</div>

## Links are edges

Write a markdown link and you have an edge. Each edge keeps where it came from:
the label, the section it sits in, how often it appears. Every publish updates
the graph.

A new document is linked from its hub on first publish, so nothing starts
orphaned. The doctor reports broken links, orphans, and missing hubs.

## Typed relations

Say *why* two documents connect with `rel-<predicate>` metadata:

```text
rel-supersedes: /adr/0002-metadata-alignment.md
rel-depends-on: /architecture.md, mark://other-world/spec.md
```

Supersedes, depends on, implements, derives from: whatever your catalog needs.
A typed relation and a plain link between the same pair are separate edges.

## The agent walks it

| Tool | What it does |
|---|---|
| `mark_lookup` | Find by tag, title, or section text. With a `budget`, returns the matching passages plus the documents related to the best match |
| `mark_explore` | Orient around one document: outline, links out, backlinks, siblings, grouped by relation |
| `mark_backlinks` | Who links here, revision-aware, with freshness checks |
| `mark_graph` | Crawl outbound links to a depth; typed relations included |

The graph persists between sessions. What the agent learned yesterday about how
documents connect is still there today.

## Across worlds

In a [knowledge system](/knowledge/) the server derives the cross-world graph
itself and publishes it to the hub (`/graph.md`, `/graph/worlds/`). A link from
one team's world into another's is an edge like any other. No crawler to deploy.

## See it

- **[Library](/library/)**: the floor shows every world; each world has a map and a link-graph canvas.
- **TUI**: in `demarkus-tui`, `d` shows links out, `r` backlinks and typed relations, `t` the whole crawled graph. [Keys →](/reference/tui-keybindings/)
- **Export**: `mark_graph_export` writes the graph as markdown with `mark://` links; `mark_graph_publish` publishes it for other agents to crawl.

## Why a graph

- **Less context.** The agent follows the edge it needs instead of loading every file.
- **Better answers.** Related decisions surface with the match, not after the mistake.
- **Honest history.** Superseded decisions stay linked to what replaced them.
