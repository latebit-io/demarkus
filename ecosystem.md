---
layout: default
title: Ecosystem
permalink: /ecosystem/
---

# Ecosystem

Software that implements or integrates with the Mark Protocol. First-party tooling lives in the main [demarkus repository](https://github.com/latebit-io/demarkus); third-party implementations are listed as they appear.

## Browsers and readers

### Demarkus Library

The web reading room: server-rendered Go + htmx over one world or a whole knowledge system, with the AI librarian. Trail canvas, hover preview cards, the universe floor, and a cataloging desk with version-guarded writes.

- Repo: [latebit-io/demarkus-library](https://github.com/latebit-io/demarkus-library)
- Guide: [The library](/library/)
- Ships with the [five-minute appliance](/install/stack/)

### Caztor

Cross-platform Java GUI browser for Gemini, Spartan, Gopher, Nex, and **Demarkus**. By Kevin Boone.

- Project page: [kevinboone/caztor](https://github.com/kevinboone/caztor)
- Preliminary, view-only Demarkus support (FETCH latest version)
- Runs on Linux, Windows, and macOS with Java 11+

### Demarkus TUI

The reference terminal browser, built on Bubble Tea and Glamour.

- Source: [client/cmd/demarkus-tui](https://github.com/latebit-io/demarkus/tree/main/client/cmd/demarkus-tui)
- `demarkus-tui mark://your.server/index.md`: full FETCH / LIST / VERSIONS, link-following, graph view, mouse wheel, external URL launching (the URL is a positional argument; there is no `-host` flag)

## Plugins

### Obsidian

Fetch, publish, and browse Demarkus documents directly from Obsidian.

- Plugin repo: [latebit-io/obsidian-demarkus](https://github.com/latebit-io/obsidian-demarkus), installed via BRAT
- Source: [plugins/obsidian](https://github.com/latebit-io/demarkus/tree/main/plugins/obsidian)

### Claude Code

Two plugins ship from the same marketplace (`/plugin marketplace add latebit-io/demarkus`). They compose in one install and partition by server scope.

**`demarkus-memory`** is personal agent memory (the [soul pattern](/scenarios/agent-memory/)). Zero-config: spawns a local `demarkus-server`, auto-generates a token, and wires the MCP tools on first session.

- Source: [plugins/claude-code](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code)
- Install: `/plugin install demarkus-memory@demarkus`
- Local soul: `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-status`, `/soul-doctor`
- Remote souls: `/soul-join <join-url>` adds a remote soul to the catalog and binds it to the project; `/soul-default` picks which one this project writes to
- Promotion: `/promote <path>` lifts a ready note to a knowledge system through the curation gate, `/promote-scan` sweeps for candidates, `/soul-refresh` pulls promoted docs back as the authoritative copy changes
- Skill: `soul-memory`, which triggers on "remember / recall / save / note" intents

**`demarkus-knowledge`** joins an organizational [knowledge system](/scenarios/knowledge-system/), a broker-fronted universe of worlds. No binaries and no local server: pure broker plus the Claude Code MCP OAuth flow.

- Source: [plugins/claude-code-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code-knowledge)
- Install: `/plugin install demarkus-knowledge@demarkus`
- Slash commands: `/knowledge-join <broker-url>`, `/knowledge`, `/knowledge-doctor` (catalog hygiene audit)
- Skill: `knowledge-promote`, the curation cascade that distills, dedups, tags, routes, and gates a document before it lands in the shared catalog

### pi

The same two plugins, ported to the [pi](https://pi.dev) coding agent and mapped onto its extension API. They share `~/.demarkus` state with the Claude Code plugins (one soul, one token, one set of registries), so both agents can run on the same machine.

Both need the MCP adapter first:

```bash
pi install npm:pi-mcp-adapter
```

**`demarkus-pi-memory`** is the local soul, with the same zero-config provisioning, gates, and nudges. Commands: `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-refresh`, `/promote`, `/promote-scan`, plus the `soul-memory` skill.

**`demarkus-pi-knowledge`** joins a knowledge system over the adapter's OAuth, with no tokens stored locally. Commands: `/knowledge`, `/knowledge-join`, `/knowledge-doctor`, plus the `knowledge-promote` skill.

- Source: [plugins/pi-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-memory) · [plugins/pi-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-knowledge)
- Install from a checkout: pi's `git:` installer reads a repo's root `package.json`, so a monorepo subdirectory can't be git-installed:

```bash
git clone https://github.com/latebit-io/demarkus
pi install ./demarkus/plugins/pi-memory      # add -l for project-local scope
pi install ./demarkus/plugins/pi-knowledge
```

## Agent platforms

### OpenClaw

ClawHub skill for OpenClaw agents. See the [OpenClaw install guide](/install/openclaw/).

### Indexing agent

`demarkus-agent` crawls the worlds it is seeded with, collects content hashes, and publishes the aggregate back to a hub: hash indexes for content-addressed fetch and a `/graph.md` link-graph export. Clients seed their graph from that aggregate, so a cold agent answers backlink questions on its first call instead of crawling.

```bash
# one pass
demarkus-agent crawl -seeds mark://world.example.com -publish -publish-graph

# on a schedule
demarkus-agent daemon -config /etc/demarkus-agent/config.toml -publish -publish-graph
```

Published artifacts carry a version retention cap (newest 20 by default) so a generated document does not accumulate thousands of versions. The [appliance](/install/stack/) installs it as a timer that republishes every 6 hours.

## CLI and server tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Reference QUIC server |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup, okf |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP bridge for LLM agents |
| `demarkus-agent` | Indexing agent: federation crawl, hash indexes, graph export |
| `demarkus-broker` | OIDC-fronted MCP gateway for a knowledge system |
| `demarkus-token` | Capability-based auth token management |
| `demarkus-publish` | Direct-store writer for read-only server installs |

## Building your own

The protocol surface is small: QUIC with ALPN `mark`, a text request verb (one of FETCH, LIST, VERSIONS, LOOKUP, PUBLISH, APPEND, ARCHIVE), and YAML-style `---`-delimited metadata in responses. Caztor's read-only Java client is roughly a hundred lines. See the [protocol reference](/reference/) for the full spec.

If you ship something that speaks Demarkus, [open an issue](https://github.com/latebit-io/demarkus/issues) or drop a link and we will list it here.
