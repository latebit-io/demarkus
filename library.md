---
layout: default
title: The Library
permalink: /library/
---

# The library

The reading room for humans. Agents read your memory and knowledge base over
MCP; people read the same documents here, in the browser, with the graph one
click away. It is a view, not another store.

- **Live: [soul.demarkus.io](https://soul.demarkus.io)**: this project's own memory, open and read-only
- Source: [latebit-io/demarkus-library](https://github.com/latebit-io/demarkus-library)

<figure class="shot">
  <img src="/assets/img/library-trail.png" alt="Two reading panes side by side: the memory index on the left, its architecture document on the right, with a trail bar across the bottom" />
  <figcaption>A trail of panes: the index beside the document it links to</figcaption>
</figure>

## Read

- **Trail.** Follow a link and it opens beside the last one. The URL carries the whole trail, so a shared link drops someone into your reading.
- **Margin.** Backlinks, trust signals, and properties per pane, on demand. Every edition one click back.
- **Hover cards.** Title, status, and opening line before you click.
- **Card catalog.** Search by tag, title, or section text, ranked by importance.
- **Copy address.** Any document or heading as a `mark://` link, ready for an agent.

<figure class="shot">
  <img src="/assets/img/library-catalog.png" alt="A search results page titled 'Catalog: broker' with a table of matching documents, their importance scores, titles, and tags" />
  <figcaption>The card catalog: a lookup for &ldquo;broker,&rdquo; ranked by importance</figcaption>
</figure>

## See the graph

**The floor** shows every world in the system. Each world has a map of its
documents and a link-graph canvas. [More on the graph →](/graph/)

<figure class="shot">
  <img src="/assets/img/library-graph.png" alt="A per-world map: a force-directed graph of the memory's documents, a central hub linked to architecture, roadmap, ADRs, and plans, with an unlinked grid below" />
  <figcaption>A world's map: linked documents above, unlinked below</figcaption>
</figure>

## Ask

The library's agent answers from the catalog: it searches, reads, follows links,
and cites what it used. Cited documents open as panes. It sees only what the
reader can see; access is enforced at the world, not in the prompt.

<figure class="shot">
  <img src="/assets/img/library-librarian.png" alt="The architecture document open on the left; on the right the agent pane answers a question about which module owns wire format parsing, citing the architecture document it read" />
  <figcaption>The library&rsquo;s agent answering from the document beside it</figcaption>
</figure>

Off until you give it a provider: any OpenAI-compatible endpoint via
`LLM_API_KEY`, `LLM_BASE_URL`, `LLM_MODEL`.

## Write

The cataloging desk creates, edits, and appends with live preview. A stale save
comes back as a merge candidate, never a silent overwrite.

## Run it

| Where | How |
|---|---|
| Knowledge system | Behind your org login, via the [knowledge-system chart](/install/stack/) |
| One Linux host | `install.sh --with-library`, read-only beside the server |
| Locally | `go run ./cmd/demarkus-library` in a clone; opens the live soul at `localhost:8080` |

Branding, themes, and every setting: the
[library README](https://github.com/latebit-io/demarkus-library#readme).
