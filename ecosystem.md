---
layout: default
title: Ecosystem
permalink: /ecosystem/
nav: docs
---

# Ecosystem

## Agent plugins

Two plugins per agent: **memory** for your own soul, **knowledge** to join a
[knowledge system](/knowledge/). Install both; they split work by server.

| Agent | Memory | Knowledge | Source |
|---|---|---|---|
| Claude Code | `demarkus-memory` | `demarkus-knowledge` | [claude-code](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code) · [claude-code-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code-knowledge) |
| Claude Cowork | `demarkus-memory` | `demarkus-knowledge` | same as Claude Code |
| Cursor | `demarkus-memory` | `demarkus-knowledge` | [cursor-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/cursor-memory) · [cursor-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/cursor-knowledge) |
| OpenCode | `demarkus-opencode-memory` | `demarkus-opencode-knowledge` | [opencode-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-memory) · [opencode-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-knowledge) |
| pi | `demarkus-pi-memory` | `demarkus-pi-knowledge` | [pi-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-memory) · [pi-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-knowledge) |

- **One core.** Every plugin runs the same `demarkus-plugin` binary and shares `~/.demarkus`: one memory, one token, one registry per machine.
- **Same commands everywhere.** `/soul-*`, `/promote*`, `/knowledge*`, plus the `remember` and `knowledge-promote` skills.
- **Your brand.** The Claude Code and Cursor plugins can ship under your own name and store noun via a `brands.json`. [Brands →](https://github.com/latebit-io/demarkus/tree/main/plugins/brands)
- **MIT licensed.** Embed them freely.

Install steps: [Memory](/memory/#install) · [Knowledge](/knowledge/#join)

## Any MCP host

Hosted memory and knowledge systems speak MCP over HTTPS with OAuth. Add the
`/mcp` URL as a connector in Claude Desktop, ChatGPT (Business, Enterprise, or
Edu with Developer mode), Cursor, or Windsurf. No plugin needed.

## Readers

| Reader | What it is |
|---|---|
| [Library](/library/) | Web reading room with the graph and an AI agent. [latebit-io/demarkus-library](https://github.com/latebit-io/demarkus-library) |
| `demarkus-tui` | Terminal browser with a persistent graph view. [Keys](/reference/tui-keybindings/) |
| [Caztor](https://github.com/kevinboone/caztor) | Java GUI browser for Gemini, Gopher, and Demarkus, by Kevin Boone. Read-only, preliminary |

## Binaries

| Binary | Purpose |
|---|---|
| `demarkus-server` | One world: your local or self-hosted soul |
| `demarkus-knowledge` | Many worlds, single sign-on, MCP gateways for knowledge and hosted memory, the cross-world graph |
| `demarkus` | CLI: fetch, publish, edit, lookup, graph, watch, okf |
| `demarkus-tui` | Terminal browser |
| `demarkus-mcp` | MCP server over one world, `lean` or `full` tool profile |
| `demarkus-plugin` | Shared core of the agent plugins; installed by them |
| `demarkus-token` | Capability tokens and join URLs |
| `demarkus-publish` | Write straight to the store for read-only installs |

## Build your own

The surface is small: QUIC with ALPN `mark`, eight text verbs, markdown with
metadata. See [the Mark Protocol](/about/). Shipped something?
[Open an issue](https://github.com/latebit-io/demarkus/issues) and we will list it.
