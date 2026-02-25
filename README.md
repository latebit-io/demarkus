# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus reimagines the web around markdown — a format structured and optimized for machines, familiar and loved by humans. Built for a world where humans and AI agents read and write together, it delivers content directly over QUIC: no rendering pipeline, no tracking, no commercialization, no unnecessary complexity. Privacy and security are foundational.

## Project Status

🟡 **Phase 2 — Read/Write MVP** — `FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, and `ARCHIVE` are all working. Auth, caching, TUI browser, MCP server, and link-graph crawler are implemented.

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

# Edit a document in $EDITOR
./client/bin/demarkus edit --insecure -auth $TOKEN mark://localhost:6309/index.md

# Browse in the terminal
./client/bin/demarkus-tui --insecure mark://localhost:6309/index.md
```

## What's Included

- **`protocol/`** — Pure Go library for the Mark Protocol wire format. Parsing, serialization, shared constants. No network code.
- **`server/`** — QUIC server with versioned document store, capability-based auth, conditional responses, path traversal protection, and hot TLS reload.
- **`client/demarkus`** — CLI for scripting and automation (`FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, `edit`, `graph`). Response caching with etag revalidation.
- **`client/demarkus-tui`** — Terminal browser with markdown rendering, link navigation, back/forward history, and document graph view.
- **`client/demarkus-mcp`** — MCP server exposing `mark_fetch`, `mark_list`, `mark_versions`, `mark_graph`, `mark_publish`, and `mark_archive` tools for LLM agents. Compatible with Claude Desktop.

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

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `PUBLISH` · `ARCHIVE`
**Deferred**: `APPEND`
**Under review**: `SEARCH`
**Status values**: `ok` · `created` · `not-modified` · `not-found` · `archived` · `unauthorized` · `not-permitted` · `server-error`

## Core Principles

1. **Optimized for Information**: Markdown is the common language — structured enough for agents, readable enough for humans
2. **Privacy First**: No user tracking, minimal logging, anonymity by default
3. **Security Minded**: Encryption mandatory, capability-based auth, secure by default
4. **Simplicity**: Human-readable protocol, minimal complexity
5. **Anti-Commercialization**: No ads, no tracking, no central authority
6. **Federation**: Anyone can run a server, content can be mirrored freely


## Interesting Use Cases. 

I been using this pattern while building demarkus, it is very unique way to handle memory between sessions, agents, and is self documenting the history of your project, the `soul` of you project so to speak. Running a local demarkus server for your project is fun and really cool. I also allow the agent to journal and reflect on its own. 

## demarkus-soul (WIP)

A living knowledge base served by demarkus itself — the agent's persistent memory across sessions. It runs as a separate demarkus server on port `6310` and is accessed via MCP.

### Running the Soul Server

The soul lives in a separate repo (`demarkus-soul`) which pulls demarkus as a dependency. See that repo for server setup. The short version:

```bash
# In the demarkus-soul repo
./server/bin/demarkus-server -root ./soul-root -port 6310
```

You will need to create a publish token with `demarkus-token` and give it root access, and config this in the mcp config. 


### Connecting

1. Build the MCP binary: `make client`
2. Copy `.mcp.json.example` to `.mcp.json` and fill in your token
3. The MCP server connects to `mark://localhost:6310`

### How to Use It

- **Start of session**: Fetch `/index.md` and key pages to load context
- **During work**: Update pages when learning something new
- **End of session**: Add a journal entry to `/journal.md` if something significant happened
- **Always**: Use `expected_version` from a prior fetch when publishing

### Content Structure

```
/index.md          — Hub page, links to all sections
/architecture.md   — System design, module boundaries, key decisions
/patterns.md       — Code patterns, conventions, idioms
/debugging.md      — Lessons from bugs and investigations
/roadmap.md        — What's done, what's next
/journal.md        — Session notes and evolution log
/guide.md          — Setup instructions
```



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
