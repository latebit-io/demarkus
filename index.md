---
layout: default
title: Demarkus
---

Your agent records decisions, lessons, and progress as it works, and recalls them in every later session. It is your second brain too: journals, logs, and notes with full history. Your team shares one versioned catalog that humans and agents keep current. The [Mark Protocol](/about/) underneath keeps it all portable and self-hostable.

Every link between documents feeds a persistent **knowledge graph**. Agents query backlinks and related documents across sessions, the [TUI](/ecosystem/#demarkus-tui) and [library](/library/) render it, and the [indexing agent](/ecosystem/#indexing-agent) aggregates it across worlds. Memory here is connected knowledge you traverse, not a pile of files.

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
pi install ./demarkus/plugins/pi-memory
```

In **[Cursor](https://cursor.com)**, add the repository as a marketplace from the plugins panel, then add the plugin:

```
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
```

All four share the same `~/.demarkus` state, so they coexist on one machine. [Agent memory →](/scenarios/agent-memory/)

Ready to share it? One command turns a fresh Linux host into a whole knowledge system: world server, broker, [reading room](/library/), self-hosted login, HTTPS, and the indexing agent.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | sudo bash
```

No DNS setup, no accounts. [The five-minute appliance →](/install/stack/)

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
    <p>One server a group reads, a reading room for humans, MCP for agents. Tokens per person, or the appliance's built-in login.</p>
    <p class="tier-links"><a href="/install/stack/">Five-minute appliance</a> · <a href="/scenarios/team/">Team knowledge base</a></p>
  </div>
  <div class="tier">
    <p class="tier-kicker">Enterprise</p>
    <h3>A knowledge system</h3>
    <p>Many worlds behind one broker, your identity provider in front. Teams join with one command; the broker mints per-world tokens.</p>
    <figure class="shot"><a href="/library/"><img src="/assets/img/library-trail.png" alt="The library reading room showing two document panes side by side" /></a></figure>
    <p class="tier-links"><a href="/scenarios/knowledge-system/">Knowledge system</a> · <a href="/library/">The library</a></p>
  </div>
</div>

Store, verbs, history, and MCP surface are identical at every tier. Only reach and identity change, so moving up is a deployment change rather than a migration. [Compare the tiers →](/scenarios/)

The tiers compose: anyone runs a personal memory and joins the org system in the same agent. The agent captures to the memory as it works, and `/promote` lifts a ready note up through a curation gate; `/memory-refresh` pulls the authoritative copy back as it changes. [How knowledge flows →](/how-it-works/)

---

## Built on the Mark Protocol

The memory and knowledge tiers sit on one foundation: the **Mark Protocol**, versioned markdown served over QUIC. It is:

- **Read-only by default**: no writes without explicit auth tokens
- **Version-preserving**: every change is kept, nothing is deleted
- **Privacy-first**: no tracking, no cookies, no user agents logged
- **Self-hostable**: one static binary, no dependencies
- **Graph-aware**: persistent document graph with backlink queries across sessions
- **OKF-compatible**: interoperates with [Open Knowledge Format](/reference/markdown/#open-knowledge-format-okf-compatibility) v0.1

The servers are **memory storage engines**: versioned, hash-chained document stores kept deliberately simple. Brokers add the service tier: identity, tenancy, and one HTTPS front door. The knowledge broker composes many worlds into an organizational system, and the memory broker serves each identity a private hosted memory from any MCP host (Claude Desktop, ChatGPT, Cursor). The intelligence lives in the agent: lookup, routing, and graph traversal.

## Tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus-knowledge-server` | Production server hosting many worlds in one process (SNI-routed, GCS-backed) |
| `demarkus-knowledge-bootstrap` | Initialize a world's GCS bucket and seed its policy |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup, okf |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP server for LLM agents: tools, resources, prompts |
| `demarkus-agent` | Indexing agent: crawls worlds, aggregates the graph, publishes hub indexes |
| `demarkus-knowledge-broker` | OIDC-fronted MCP gateway composing many worlds into one system |
| `demarkus-memory-broker` | OIDC-fronted MCP gateway serving one private memory world per identity (memory as a service) |
| `demarkus-library` | Web reading room with the AI librarian |
| `demarkus-token` | Generate capability-based auth tokens |
| `demarkus-publish` | Write directly to the store for read-only server installs |
| `demarkus-migrate` | Migrate document history between store backends |

## Install

- [Five-minute appliance](/install/stack/): the whole system on one host
- [macOS](/install/macos/) · [Linux](/install/linux/) · [Windows (WSL2)](/install/windows/) · [Docker](/install/docker/)

## Learn more

- [About the Mark Protocol](/about/)
- [The library and the librarian](/library/)
- [Security model](/security/): attack surface, hardening, threat model
- [Ecosystem](/ecosystem/): browsers, plugins, and tools that speak Demarkus
- [Memory: the project's own live memory](/memory/)
- [Public hub](https://github.com/latebit-io/demarkus-hub): discovery index at `mark://hub.demarkus.io`
- [GitHub](https://github.com/latebit-io/demarkus)
