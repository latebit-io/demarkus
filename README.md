# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus implements the Mark Protocol — versioned markdown served over QUIC. No rendering pipeline, no tracking, no central authority. Read and write with capability tokens. Every change is permanent.

## Install

```bash
# macOS / Linux — server + client
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash

# Client only (CLI, TUI, MCP)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

See [full install docs](https://latebit-io.github.io/demarkus/getting-started/) for platform-specific guides.

## Quick Start

```bash
# Fetch a document from the live soul server
demarkus mark://soul.demarkus.io/index.md

# Browse interactively
demarkus-tui mark://soul.demarkus.io/index.md

# Run your own server (dev mode — self-signed cert)
demarkus-server -root ./docs/site

# Fetch from your local server
demarkus --insecure mark://localhost:6309/index.md

# Generate a write token
demarkus-token generate -paths "/*" -ops publish -tokens ./tokens.toml

# Publish a document
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/hello.md -body "# Hello"

# Edit in $EDITOR, publish on save
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/hello.md
```

## What's Included

| Binary | Purpose |
|--------|---------|
| `demarkus-server` | QUIC server with versioned document store, capability-based auth, hot TLS reload |
| `demarkus-token` | Generate and manage capability tokens |
| `demarkus` | CLI: `FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, `APPEND`, `ARCHIVE`, `edit`, `graph`, `info` |
| `demarkus-tui` | Terminal browser with markdown rendering, link navigation, history, graph view |
| `demarkus-mcp` | MCP server exposing all protocol verbs as tools for LLM agents |

## Protocol at a Glance

**Transport**: QUIC (UDP 6309) | **Scheme**: `mark://` | **Content**: Markdown + YAML frontmatter

Request:
```
FETCH /hello.md
```

Response:
```
---
status: ok
version: 3
modified: 2026-01-15T10:30:00Z
---

# Hello World
```

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `PUBLISH` · `APPEND` · `ARCHIVE`

## Use Cases

### Agent Memory (demarkus-soul)

Run a Demarkus server as an AI agent's persistent memory across sessions. The agent reads context at session start and writes updates as work progresses — architecture notes, debugging lessons, journal entries, thoughts.

The Demarkus project uses this pattern live at `mark://soul.demarkus.io`. Browse it:

```bash
demarkus-tui mark://soul.demarkus.io/index.md
```

See the [Agent Memory guide](https://latebit-io.github.io/demarkus/scenarios/agent-memory/) for setup instructions.

### Personal Knowledge Base

Run a local server, publish markdown through the protocol, browse with the TUI. Every document is versioned from the first write.

### Public Documentation Hub

Deploy on a VPS with Let's Encrypt. Open read access, token-gated writes. One command:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain yourdomain.com --root /srv/site
```

### Demarkus Hubs

A hub server links to content on other Demarkus servers — a curated directory, not original content. Hubs link to hubs, creating a navigable hierarchy of knowledge. All versioned, all verifiable, no central registry.

## Build from Source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all   # or: make server / make client
```

Requires Go 1.22+. Binaries land in `server/bin/` and `client/bin/`.

## Documentation

- [Website](https://latebit-io.github.io/demarkus/) — install guides, scenarios, troubleshooting
- [Protocol Specification](docs/SPEC.md) — complete wire format
- [Design Rationale](docs/DESIGN.md) — why things are built the way they are

## Core Principles

1. **Optimized for Information** — Markdown is the common language: structured enough for agents, readable enough for humans
2. **Privacy First** — No user tracking, minimal logging, anonymity by default
3. **Security Minded** — Encryption mandatory, capability-based auth, secure by default
4. **Simplicity** — Human-readable protocol, minimal complexity
5. **Anti-Commercialization** — No ads, no tracking, no central authority
6. **Federation** — Anyone can run a server, content can be mirrored freely

## License

- Implementation: AGPL-3.0-only ([LICENSE](LICENSE))
- Protocol Specification: CC0-1.0 ([LICENSE-PROTOCOL](LICENSE-PROTOCOL))

---

*"The web we want, not the web we got."*
