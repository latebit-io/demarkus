---
layout: default
title: Demarkus
---

Your agent records decisions, lessons, and progress as it works, and recalls them in every later session. It is your second brain too: journals, logs, and notes with full history. Your team shares one versioned catalog that humans and agents keep current. The [Mark Protocol](/about/) underneath keeps it all portable and self-hostable.

Every link between documents feeds a persistent **knowledge graph**. Agents query backlinks and related documents across sessions, the [TUI](/ecosystem/#demarkus-tui) and [library](/library/) render it, and the [knowledge server](/ecosystem/#knowledge-server) derives it across worlds. Memory here is connected knowledge you traverse, not a pile of files.

<div class="memory-demo memory-demo--wide">
  <img src="/demarkus-soul-graph.svg" alt="Knowledge graph of soul.demarkus.io: 261 documents and 840 edges, clustered by prefix, with typed relations highlighted" />
  <p class="memory-demo-caption">The project's own memory, crawled and drawn by an agent in one mark_graph call</p>
</div>

## Start here

Give your agent memory. There is nothing to configure: the plugin spawns a local server, mints its own token, and wires up the tools. In **Claude Code**:

```
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

In **[OpenCode](https://opencode.ai)**:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

In **[pi](https://pi.dev)**:

```bash
pi install npm:pi-mcp-adapter
pi install git:github.com/latebit-io/demarkus-pi-memory
```

In **[Cursor](https://cursor.com)**, add the repository as a marketplace from the plugins panel, then add the plugin:

```
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
```

All four share the same `~/.demarkus` state, so they coexist on one machine. [Agent memory →](/scenarios/agent-memory/)

Ready to share it? Run a [shared world](/scenarios/team/), or deploy a [knowledge system](/install/stack/) with many worlds, single sign-on, and the [reading room](/library/).

---

## One system, three scales

<div class="tiers">
  <div class="tier">
    <p class="tier-kicker">Personal</p>
    <h3>Your memory</h3>
    <p>Your agent's memory and your notes on a server of your own, local or remote, that you join from every device.</p>
    <p class="tier-links"><a href="/scenarios/agent-memory/">Agent memory</a> · <a href="/scenarios/personal-wiki/">Personal wiki</a></p>
  </div>
  <div class="tier">
    <p class="tier-kicker">Team</p>
    <h3>A shared world</h3>
    <p>One server a group reads, a reading room for humans, MCP for agents. Capability tokens scope each person's writes.</p>
    <p class="tier-links"><a href="/scenarios/team/">Team knowledge base</a> · <a href="/library/">The library</a></p>
  </div>
  <div class="tier">
    <p class="tier-kicker">Enterprise</p>
    <h3>A knowledge system</h3>
    <p>Many worlds behind one gateway, your identity provider in front. Teams join with one command; access is scoped per world.</p>
    <figure class="shot"><a href="/library/"><img src="/assets/img/library-trail.png" alt="The library reading room showing two document panes side by side" /></a></figure>
    <p class="tier-links"><a href="/scenarios/knowledge-system/">Knowledge system</a> · <a href="/library/">The library</a></p>
  </div>
</div>

One versioned document model at every tier. Storage, identity, and discovery scale with the deployment. [Compare the tiers →](/scenarios/)

The tiers compose: anyone runs a personal memory and joins the org system in the same agent. The agent captures to the memory as it works, and `/promote` lifts a ready note up through a curation gate; `/soul-refresh` reconciles it with the shared copy. [How knowledge flows →](/how-it-works/)

---

## Built on the Mark Protocol

The memory and knowledge tiers sit on one foundation: the **Mark Protocol**, versioned markdown served over QUIC. It is:

- **Read-only by default**: no writes without explicit auth tokens
- **Version-preserving**: history is kept unless explicitly pruned with retention
- **Privacy-first**: no tracking, no cookies, no user agents logged
- **Self-hostable**: one static binary, no dependencies
- **Graph-aware**: persistent document graph with backlink queries across sessions
- **OKF-compatible**: interoperates with [Open Knowledge Format](/reference/markdown/#open-knowledge-format-okf-compatibility) v0.1

The servers are **memory storage engines**: versioned, hash-chained document stores. `demarkus-knowledge` adds identity, tenancy, and an HTTPS front door in the same process. Its knowledge gateway serves a shared catalog; its memory gateway gives each identity a private world. Agents decide what to recall, record, and promote.

## Tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus-knowledge` | Many GCS-backed worlds, OIDC gateways, and the cross-world graph in one process |
| `demarkus-knowledge-server` | Multi-world QUIC server without the OIDC gateways |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup, okf |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP server for LLM agents: tools, resources, prompts |
| `demarkus-library` | Web reading room with the AI librarian |
| `demarkus-token` | Generate capability-based auth tokens |
| `demarkus-publish` | Write directly to the store for read-only server installs |

## Install

- [Knowledge system](/install/stack/): worlds, gateways, and the library with Helm
- [macOS](/install/macos/) · [Linux](/install/linux/) · [Windows (WSL2)](/install/windows/) · [Docker](/install/docker/)

## Learn more

- [About the Mark Protocol](/about/)
- [The library and the librarian](/library/)
- [Security model](/security/): attack surface, hardening, threat model
- [Ecosystem](/ecosystem/): browsers, plugins, and tools that speak Demarkus
- [Memory: the project's own live memory](/memory/)
- [Public hub](https://github.com/latebit-io/demarkus-hub): discovery index at `mark://hub.demarkus.io`
- [GitHub](https://github.com/latebit-io/demarkus)
