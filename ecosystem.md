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
- `demarkus-tui -host mark://your.server/` — full FETCH / LIST / VERSIONS, link-following, graph view, mouse wheel, external URL launching

## Plugins

### Obsidian

Fetch, publish, and browse Demarkus documents directly from Obsidian.

- Plugin repo: [latebit-io/obsidian-demarkus](https://github.com/latebit-io/obsidian-demarkus) — install via BRAT
- Source: [plugins/obsidian](https://github.com/latebit-io/demarkus/tree/main/plugins/obsidian)

### Claude Code

Zero-config plugin for Claude Code: spawns a local `demarkus-server`, auto-generates a token, and wires the MCP tools on first session.

- Source: [plugins/claude-code](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code)
- Install: `/plugin marketplace add latebit-io/demarkus` then `/plugin install demarkus-memory@demarkus`
- Slash commands: `/soul`, `/soul-journal`, `/soul-init`
- Skill: `memory` — triggers on "remember / recall / save / note" intents

## Agent platforms

### OpenClaw

ClawHub skill for OpenClaw agents. See the [OpenClaw install guide](/install/openclaw/).

## CLI and server tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Reference QUIC server |
| `demarkus` | CLI: fetch, list, publish, edit, graph |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP bridge for LLM agents |
| `demarkus-token` | Capability-based auth token management |
| `demarkus-publish` | Direct-store writer for read-only server installs |

## Building your own

The protocol surface is small: QUIC with ALPN `mark`, a text request verb (one of FETCH, LIST, VERSIONS, LOOKUP, PUBLISH, APPEND, ARCHIVE), and YAML-style `---`-delimited metadata in responses. Caztor's read-only Java client is roughly a hundred lines. See the [protocol reference](/reference/) for the full spec.

If you ship something that speaks Demarkus, [open an issue](https://github.com/latebit-io/demarkus/issues) or drop a link and we will list it here.
