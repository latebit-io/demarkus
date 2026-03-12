---
layout: default
title: Demarkus
---

Demarkus is a protocol and toolkit for publishing markdown documents with version history, capability-based access, and QUIC transport.

**What do you want to do?**

- [Set up agent memory - soul](/scenarios/agent-memory/) — persistent memory for Claude Code and other LLM agents
- [Install on macOS](/install/macos/) — one-line install, works today
- [Install on Linux](/install/linux/) — server + client via install script
- [Install with Docker](/install/docker/) — multi-arch image, docker-compose ready
- [Install on Windows (WSL2)](/install/windows/) — Windows setup via WSL2
- [Install via OpenClaw](/install/openclaw/) — ClawHub skill for OpenClaw agents
- [Install Obsidian plugin](https://github.com/latebit-io/obsidian-demarkus) — fetch and publish from Obsidian via BRAT
- [Run a personal knowledge base](/scenarios/personal-wiki/) — local markdown notes, browsable via TUI
- [Publish a public hub](/scenarios/public-hub/) — VPS + Let's Encrypt + open read access
- [Set up a team knowledge base](/scenarios/team/) — shared server with token-based write access

---

## What Demarkus is

Demarkus implements the **Mark Protocol** — a minimal protocol for serving versioned markdown over QUIC. It is:

- **Read-only by default** — no writes without explicit auth tokens
- **Version-preserving** — every change is kept, nothing is deleted
- **Privacy-first** — no tracking, no cookies, no user agents logged
- **Self-hostable** — runs on a $30 single-board computer
- **Graph-aware** — persistent document graph with backlink queries across sessions

## Tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus` | CLI: fetch, list, publish, edit, graph |
| `demarkus-tui` | Interactive terminal browser with graph view |
| `demarkus-mcp` | MCP server for LLM agents (graph, backlinks, discovery) |
| `demarkus-token` | Generate capability-based auth tokens |

## Learn more

- [About the Mark Protocol](/about/)
- [Soul — the project's live AI memory](/soul/)
- [Public Hub](https://github.com/latebit-io/demarkus-hub) — discovery index at `mark://hub.demarkus.io`
- [GitHub](https://github.com/latebit-io/demarkus)
