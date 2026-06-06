---
layout: default
title: Ecosystem
permalink: /ecosystem/
---

# Ecosystem

Software that implements or integrates with the Mark Protocol. First-party tooling lives in the main [demarkus repository](https://github.com/latebit-io/demarkus); third-party implementations are listed as they appear.

## Browsers and readers

### Caztor

Cross-platform Java GUI browser for Gemini, Spartan, Gopher, Nex, and **Demarkus**. By Kevin Boone.

- Project page: [kevinboone/caztor](https://github.com/kevinboone/caztor)
- Preliminary, view-only Demarkus support (FETCH latest version)
- Runs on Linux, Windows, and macOS with Java 11+

### Demarkus TUI

The reference terminal browser, built on Bubble Tea and Glamour.

- Source: [client/cmd/demarkus-tui](https://github.com/latebit-io/demarkus/tree/main/client/cmd/demarkus-tui)
- `demarkus-tui mark://your.server/index.md` — full FETCH / LIST / VERSIONS, link-following, graph view, mouse wheel, external URL launching (the URL is a positional argument; there is no `-host` flag)

## Plugins

### Obsidian

Fetch, publish, and browse Demarkus documents directly from Obsidian.

- Plugin repo: [latebit-io/obsidian-demarkus](https://github.com/latebit-io/obsidian-demarkus) — install via BRAT
- Source: [plugins/obsidian](https://github.com/latebit-io/demarkus/tree/main/plugins/obsidian)

### Claude Code

Two plugins ship from the same marketplace (`/plugin marketplace add latebit-io/demarkus`). They compose in one install and partition by server scope.

**`demarkus-memory`** — personal agent memory (the [soul pattern](/scenarios/agent-memory/)). Zero-config: spawns a local `demarkus-server`, auto-generates a token, and wires the MCP tools on first session.

- Source: [plugins/claude-code](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code)
- Install: `/plugin install demarkus-memory@demarkus`
- Slash commands: `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-status`, `/soul-doctor`
- Skill: `soul-memory` — triggers on "remember / recall / save / note" intents

**`demarkus-knowledge`** — join an organizational [knowledge system](/scenarios/knowledge-system/), a broker-fronted universe of worlds. No binaries and no local server: pure broker plus the Claude Code MCP OAuth flow.

- Source: [plugins/claude-code-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code-knowledge)
- Install: `/plugin install demarkus-knowledge@demarkus`
- Slash commands: `/knowledge-join <broker-url>`, `/knowledge`

## Agent platforms

### OpenClaw

ClawHub skill for OpenClaw agents. See the [OpenClaw install guide](/install/openclaw/).

## CLI and server tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Reference QUIC server |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP bridge for LLM agents |
| `demarkus-token` | Capability-based auth token management |
| `demarkus-publish` | Direct-store writer for read-only server installs |

## Building your own

The protocol surface is small: QUIC with ALPN `mark`, a text request verb (one of FETCH, LIST, VERSIONS, LOOKUP, PUBLISH, APPEND, ARCHIVE), and YAML-style `---`-delimited metadata in responses. Caztor's read-only Java client is roughly a hundred lines. See the [protocol reference](/reference/) for the full spec.

If you ship something that speaks Demarkus, [open an issue](https://github.com/latebit-io/demarkus/issues) or drop a link and we will list it here.
