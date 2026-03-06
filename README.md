# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus reimagines the web around markdown — a format structured and optimized for machines, familiar and loved by humans. Built for a world where humans and AI agents read and write together, it delivers content directly over QUIC: no rendering pipeline, no tracking, no commercialization, no unnecessary complexity. Privacy and security are foundational.

## Project Status

🟡 **Phase 2 — Read/Write MVP** — `FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, `APPEND`, and `ARCHIVE` are all working. Auth, caching, TUI browser, MCP server, and link-graph crawler are implemented.

## Quick Start

```bash
# Prerequisites: Go 1.26+, Make (optional)

# Build everything
make all

# Start a server (dev mode — self-signed cert)
./server/bin/demarkus-server -root ./docs/site

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
- **`client/demarkus-mcp`** — MCP server exposing `mark_fetch`, `mark_list`, `mark_versions`, `mark_graph`, `mark_publish`, `mark_append`, and `mark_archive` tools for LLM agents. Compatible with Claude Desktop.

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

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `PUBLISH` · `APPEND` · `ARCHIVE`
**Status values**: `ok` · `created` · `not-modified` · `not-found` · `archived` · `unauthorized` · `not-permitted` · `server-error`

## Core Principles

1. **Optimized for Information**: Markdown is the common language — structured enough for agents, readable enough for humans
2. **Privacy First**: No user tracking, minimal logging, anonymity by default
3. **Security Minded**: Encryption mandatory, capability-based auth, secure by default
4. **Simplicity**: Human-readable protocol, minimal complexity
5. **Anti-Commercialization**: No ads, no tracking, no central authority
6. **Federation**: Anyone can run a server, content can be mirrored freely


## Interesting Use Cases

### demarkus-soul (Agent Memory)

I've been using this pattern while developing demarkus with Claude and Codex. It's a unique way to handle memory between sessions and agents, and is self-documenting the history of your project — the `soul` of your project, so to speak. Running a local demarkus server for your project is fun and really cool. I also allow the agent to journal and reflect on its own.

### Demarkus Hubs (Curated Link Directories)

A demarkus hub is a server whose sole purpose is linking to content on other demarkus servers. No original content — just curated collections of `mark://` links organized by topic. Think of it as a librarian, not a library.

Anyone can run a hub. A hub for Go documentation links to `mark://go-docs.example.com/...`. A community hub links to everything its members find valuable. Hubs can link to other hubs, creating a navigable hierarchy of curated knowledge — all versioned, all verifiable, no central registry needed.

## demarkus-soul (WIP)

A living knowledge base served by demarkus itself — the agent's persistent memory across sessions. It runs as a separate demarkus server on port `6310` and is accessed via MCP.

### Quick Start: Writing Documents

Documents in demarkus-soul are **published over the protocol**, not added to the filesystem. Here's the workflow:

1. **Generate a publish token** (one-time, required)
   ```bash
   # In the soul's content root
   ./tools/bin/demarkus-token generate -paths "/*" -ops publish,archive -tokens /path/to/soul-content/tokens.toml
   ```
   Save the raw token output — it's shown once.

2. **Publish a new document** using the CLI or MCP
   ```bash
   # CLI: publish a new doc
   ./client/bin/demarkus publish --insecure -auth $TOKEN mark://localhost:6310/mypage.md < content.md

   # MCP: agents use mark_publish (recommended for automation)
   mark_publish with expected_version=0 to create, or fetch first to get current version
   ```

3. **Update an existing document** using `mark_publish` or `mark_append`
   ```bash
   # CLI: append to existing doc (minimal, efficient for journals)
   ./client/bin/demarkus append --insecure -auth $TOKEN mark://localhost:6310/journal.md < entry.md
   ```

**Key point**: Documents are versioned, immutable records on the server. You must always use `expected_version` from a prior fetch when publishing/appending to detect conflicts.

### Running the Soul Server

The soul lives in a separate repo (`demarkus-soul`) which pulls demarkus as a dependency. See that repo for full setup. The short version:

```bash
# In the demarkus-soul repo
./server/bin/demarkus-server -root ./soul-root -port 6310
```

### Connecting Claude Code

1. Build the MCP binary: `make client`
2. **Generate a publish token** (see step 1 above if not done)
3. Copy `.mcp.json.example` to `.mcp.json` and configure the token:
   ```json
   {
     "mcpServers": {
       "demarkus-soul": {
         "command": "/path/to/client/bin/demarkus-mcp",
         "args": ["-host", "mark://localhost:6310", "-token", "<your-token>", "-insecure"]
       }
     }
   }
   ```
4. Claude Code agents can now use `mark_fetch`, `mark_publish`, `mark_append`, etc.

**Without a token**, agents can only read documents. Write access (`mark_publish`, `mark_append`) requires the token.

### How to Use It

- **Start of session**: Fetch `/index.md` and key pages to load context
- **During work**: Update pages when learning something new (use `mark_publish` for full updates, `mark_append` for journals)
- **End of session**: Add a journal entry to `/journal.md` if something significant happened
- **Always**: Use `expected_version` from a prior fetch when publishing/appending

### Content Structure

```
/index.md          — Hub page, links to all sections
/architecture.md   — System design, module boundaries, key decisions
/patterns.md       — Code patterns, conventions, idioms
/debugging.md      — Lessons from bugs and investigations
/roadmap.md        — What's done, what's next
/journal.md        — Session notes and evolution log
/guide.md          — Full setup guide for agents
```

**Full guide**: See `/guide.md` on the soul for complete setup instructions, including MCP configuration and available tools.



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

- Implementation Code: AGPL-3.0-only (see [LICENSE](LICENSE))
- Protocol Specification and related protocol-definition docs: CC0-1.0 (see [LICENSE-PROTOCOL](LICENSE-PROTOCOL))

---

*"The web we want, not the web we got."*
