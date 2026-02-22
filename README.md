# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus reimagines the web around markdown — a format structured and optimized for machines, familiar and loved by humans. Built for a world where humans and AI agents read and write together, it delivers content directly over QUIC: no rendering pipeline, no tracking, no commercialization, no unnecessary complexity. Privacy and security are foundational.

## Project Status

🟡 **Phase 2 — Read/Write MVP** — `FETCH`, `LIST`, `VERSIONS`, and `PUBLISH` are all working. Auth, caching, TUI browser, MCP server, and link-graph crawler are implemented.

## Quick Start

```bash
# Prerequisites: Go 1.26+, Make (optional)

# Build everything
make all

# Start a server (dev mode — self-signed cert)
./server/bin/demarkus-server -root ./examples/demo-site

# Fetch a document
./client/bin/demarkus --insecure mark://localhost:6309/index.md

# List a directory
./client/bin/demarkus --insecure -X LIST mark://localhost:6309/

# Browse in the terminal
./client/bin/demarkus-tui --insecure mark://localhost:6309/index.md
```

## What's Included

- **`protocol/`** — Pure Go library for the Mark Protocol wire format. Parsing, serialization, shared constants. No network code.
- **`server/`** — QUIC server with versioned document store, capability-based auth, conditional responses, path traversal protection, and hot TLS reload.
- **`client/demarkus`** — CLI for scripting and automation (`FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, `graph`). Response caching with etag revalidation.
- **`client/demarkus-tui`** — Terminal browser with markdown rendering, link navigation, back/forward history, and document graph view.
- **`client/demarkus-mcp`** — MCP server exposing `mark_fetch`, `mark_list`, `mark_graph`, and `mark_publish` tools for LLM agents. Compatible with Claude Desktop.

## Protocol at a Glance

**Transport**: QUIC (UDP port 6309) | **Scheme**: `mark://` | **Content**: Markdown with YAML frontmatter

Request — a single newline-terminated line:
```
FETCH /hello.md
```

Response — YAML frontmatter + markdown body:
```
---
status: ok
modified: 2025-02-14T10:30:00Z
version: 1
---

# Hello World

Welcome to Demarkus!
```

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `PUBLISH`
**Planned**: `APPEND` · `ARCHIVE` · `SEARCH`
**Status values**: `ok` · `created` · `not-modified` · `not-found` · `unauthorized` · `not-permitted` · `server-error`

## Core Principles

1. **Optimized for Information**: Markdown is the common language — structured enough for agents, readable enough for humans
2. **Privacy First**: No user tracking, minimal logging, anonymity by default
3. **Security Minded**: Encryption mandatory, capability-based auth, secure by default
4. **Simplicity**: Human-readable protocol, minimal complexity
5. **Anti-Commercialization**: No ads, no tracking, no central authority
6. **Federation**: Anyone can run a server, content can be mirrored freely

## Documentation

Browse the docs over the protocol itself:
```bash
./client/bin/demarkus-tui mark://demarkus.latebit.io/index.md
```

Or read locally:
- **[Full Documentation](docs/site/)** — install, server setup, client tools, deployment, architecture, and more
- **[Protocol Specification](docs/SPEC.md)** — the complete wire format spec
- **[Design Rationale](docs/DESIGN.md)** — why things are built the way they are

## Contributing

Early-stage development. The protocol specification is still evolving. Contributions, feedback, and critiques are welcome!

## License

- Protocol Specification: CC0 (Public Domain)
- Implementation Code: MIT (TBD)

---

*"The web we want, not the web we got."*
