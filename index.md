---
layout: default
title: Demarkus
---

Demarkus is a protocol and toolkit for publishing markdown documents with version history, capability-based access, and QUIC transport.

**What do you want to do?**

- [Install on macOS](/install/macos/) — one-line install, works today
- [Install on Linux](/install/linux/) — server + client via install script
- [Install with Docker](/install/docker/) — multi-arch image, docker-compose ready
- [Install on Windows (WSL2)](/install/windows/) — Windows setup via WSL2
- [Run a personal knowledge base](/scenarios/personal-wiki/) — local markdown notes, browsable via TUI
- [Set up agent memory](/scenarios/agent-memory/) — persistent memory for Claude Code and other LLM agents
- [Publish a public hub](/scenarios/public-hub/) — VPS + Let's Encrypt + open read access
- [Set up a team knowledge base](/scenarios/team/) — shared server with token-based write access

---

## What Demarkus is

Demarkus implements the **Mark Protocol** — a minimal protocol for serving versioned markdown over QUIC. It is:

- **Read-only by default** — no writes without explicit auth tokens
- **Version-preserving** — every change is kept, nothing is deleted
- **Privacy-first** — no tracking, no cookies, no user agents logged
- **Self-hostable** — runs on a $30 single-board computer

## Tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus` | CLI: fetch, list, publish, edit |
| `demarkus-tui` | Interactive terminal browser |
| `demarkus-mcp` | MCP server for LLM agents |
| `demarkus-token` | Generate capability-based auth tokens |

## Learn more

- [About the Mark Protocol](/about/)
- [Soul — the project's live AI memory](/soul/)
- [GitHub](https://github.com/latebit-io/demarkus)
