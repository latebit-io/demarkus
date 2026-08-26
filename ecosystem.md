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
- Recall: session guidance starts shared lookups with `mark_lookup_all`, the broker's one-call catalog search across every readable world

### OpenCode

The OpenCode ports provide the same memory and knowledge-system workflows and
share `~/.demarkus` state with the Claude Code and pi plugins.

**`demarkus-opencode-memory`** installs local soul support:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

**`demarkus-opencode-knowledge`** joins an organizational knowledge system:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash
```

- Source: [plugins/opencode-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-memory) · [plugins/opencode-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-knowledge)
- Commands: the same `/soul*`, `/promote*`, and `/knowledge*` command sets described above
- Restart OpenCode after installation; restart once more after memory's first session or after joining a knowledge system so the new MCP entry connects

### pi

The same two plugins, ported to the [pi](https://pi.dev) coding agent and mapped onto its extension API. They share `~/.demarkus` state with the Claude Code and OpenCode plugins (one soul, one token, one set of registries), so all three agents can run on the same machine.

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

### Indexing agent

`demarkus-agent` crawls the worlds it is seeded with, collects content hashes, and publishes the aggregate back to a hub: sharded hash indexes for content-addressed fetch and an atomic sharded graph snapshot under `/graph/manifest.md`, both staged in A/B slots so readers never see a partial write. Crawls walk large directories with paginated LIST cursors. Clients seed their graph from that aggregate, so a cold agent answers backlink questions on its first call instead of crawling.

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
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus-knowledge-server` | Production server hosting many worlds in one process (SNI-routed, GCS-backed) |
| `demarkus-knowledge-bootstrap` | Initialize a world's GCS bucket and seed its policy |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup, okf |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP server for LLM agents: tools, resources, prompts |
| `demarkus-agent` | Indexing agent: crawls worlds, aggregates the graph, publishes hub indexes |
| `demarkus-broker` | OIDC-fronted MCP gateway composing many worlds into one system |
| `demarkus-library` | Web reading room with the AI librarian |
| `demarkus-token` | Generate capability-based auth tokens |
| `demarkus-publish` | Write directly to the store for read-only server installs |
| `demarkus-migrate` | Migrate document history between store backends |

## Building your own

The protocol surface is small: QUIC with ALPN `mark`, a text request verb (one of FETCH, LIST, VERSIONS, LOOKUP, PUBLISH, APPEND, ARCHIVE), and YAML-style `---`-delimited metadata in responses. Caztor's read-only Java client is roughly a hundred lines. See the [protocol reference](/reference/) for the full spec.

If you ship something that speaks Demarkus, [open an issue](https://github.com/latebit-io/demarkus/issues) or drop a link and we will list it here.
