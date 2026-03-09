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

See [full install docs](https://latebit-io.github.io/demarkus/getting-started/) for platform-specific guides and other options.

### See it in action

![Client install demo](docs/images/demarkus-client.gif)

## Quick Start

```bash
# Fetch a document from the live soul server
demarkus mark://soul.demarkus.io/index.md

# Browse interactively with the TUI
demarkus-tui mark://soul.demarkus.io/index.md

# Run your own local server
demarkus-server -root ./docs/site
```

For more examples (tokens, publishing, editing), see [full usage guide](https://latebit-io.github.io/demarkus/).

## What's Included

| Binary | Purpose |
|--------|---------|
| `demarkus-server` | QUIC server with versioned document store, capability-based auth |
| `demarkus-token` | Generate and manage write tokens |
| `demarkus` | CLI tool for all protocol operations (fetch, publish, append, graph, etc.) |
| `demarkus-tui` | Terminal browser: markdown rendering, link navigation, persistent graph |
| `demarkus-mcp` | MCP server for LLM agents (protocol verbs + graph crawling, backlinks, indexing) |

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

**Agent Memory** — Run a server as persistent memory across agent sessions. The Demarkus project itself uses this pattern at `mark://soul.demarkus.io` for architecture notes, debugging lessons, and journal entries.

**Personal Knowledge Base** — Local server, versioned documents, TUI browser. Everything from first write.

**Public Documentation** — Deploy on a VPS, share links, gate writes with tokens.

**Demarkus Hubs** — Link to content on other servers, building a federated directory of knowledge. Hubs can link to hubs.

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
