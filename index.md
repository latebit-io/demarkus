---
layout: default
---

One store for what your agent learns and what your team knows: plain markdown,
every version kept, every link an edge. Local by default, shared when ready.

## Start here

Give your agent memory. Nothing to configure: the plugin starts a local server
and wires up the tools. In **Claude Code**:

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

Also for [Cursor, Cowork, OpenCode, and pi](/install/). All share one memory on
your machine.

<div class="tiers">
  <div class="tier">
    <p class="tier-kicker">Memory</p>
    <h3>Your agent remembers</h3>
    <p>Decisions, bug lessons, patterns, and a daily journal, filed as it works. Recalled before the next task, so no session starts cold.</p>
    <p class="tier-links"><a href="/memory/">Memory →</a></p>
  </div>
  <div class="tier">
    <p class="tier-kicker">Knowledge base</p>
    <h3>Your team knows</h3>
    <p>One shared catalog behind your login. Notes arrive from memory through a review gate; policy blocks secrets and duplicates on every write.</p>
    <p class="tier-links"><a href="/knowledge/">Knowledge →</a></p>
  </div>
  <div class="tier">
    <p class="tier-kicker">Graph</p>
    <h3>It all connects</h3>
    <p>Links and typed relations form a persistent graph. Agents follow edges instead of loading files; people browse it in the library.</p>
    <p class="tier-links"><a href="/graph/">Graph →</a></p>
  </div>
</div>

<div class="memory-demo memory-demo--wide">
  <img src="/demarkus-soul-graph.svg" alt="Knowledge graph of soul.demarkus.io: 261 documents and 840 edges, clustered by prefix, with typed relations highlighted" />
  <p class="memory-demo-caption">This project's own memory, crawled and drawn by an agent in one mark_graph call</p>
</div>

## How it works

```text
  agent ──record──▶ MEMORY ──/promote──▶ KNOWLEDGE BASE ──▶ LIBRARY
    ▲               private     review     shared, governed    people
    └──────────── recall · consult · follow the graph ◀────────┘
```

- **Record as you go.** The agent files each note where it belongs. Hooks catch untagged writes and missing journal entries.
- **Recall with evidence.** Lookup returns the matching section and its related documents, not whole files.
- **Promote what matters.** `/promote` distills a note for others; you approve before it lands.
- **Keep every version.** Nothing is overwritten. Every change has an author and a history.

## Live

- **[soul.demarkus.io](https://soul.demarkus.io)**: this project's own agent memory, open to read. [About it →](/memory/#live-example)
- **knowledge.demarkus.io**: a live knowledge system. `/knowledge-join https://knowledge.demarkus.io`

## Yours to run

Local by default. Self-host a server in one binary, or a whole knowledge system
with one Helm chart. No tracking, no lock-in: it is all markdown, on the open
[Mark Protocol](/about/).

[Install →](/install/) · [Docs →](/reference/) · [GitHub →](https://github.com/latebit-io/demarkus)
