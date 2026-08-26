# Architecture & Design

This page describes the Demarkus system architecture, key design decisions, and how the protocol, server, and clients fit together. It is intended as a practical overview for operators, contributors, and integrators.

## System Overview

Demarkus is a markdown‑native document protocol built on QUIC. The repository is organized as six modules:

- **`protocol/`** — wire format and parsing/serialization only
- **`server/`** — QUIC servers: `demarkus-server` (filesystem), `demarkus-server-pg` (Postgres), `demarkus-knowledge-server` (multi-world, GCS)
- **`client/`** — CLI, TUI, MCP server, and the federation agent
- **`tools/`** — the OIDC broker/MCP gateway, token tooling, direct-to-store publish, load testing
- **`plugins/`** — agent plugins for Claude Code, OpenCode, pi, and Obsidian
- **`deploy/`** — Helm charts, Kubernetes examples, and the kind dev harness

The architecture intentionally separates **protocol parsing** from **transport**, and **storage** from **network**, so that each layer can be tested independently.

## Components

### Protocol (Pure Wire Format)

The protocol module defines:

- Request format: newline‑terminated lines (`FETCH /path.md\n`)
- Response format: YAML frontmatter + markdown body
- Constants: default port, ALPN, verbs, status values

It contains **no networking code** and **no filesystem access**.

### Server

The server:

- Accepts QUIC connections over TLS
- Parses incoming protocol requests
- Reads/writes documents from a versioned content store
- Emits protocol responses with frontmatter + markdown body

Core server pieces:

- **Handler**: request parsing, verb routing, response formatting
- **Store**: versioned document storage with hash‑chain integrity; backends are the filesystem (default), Postgres (`demarkus-server-pg`), and GCS (`demarkus-knowledge-server`)
- **Auth**: capability‑based token verification (hashes only)
- **TLS**: dev cert generation, prod cert loading, SIGHUP reload

### Clients

The client module ships four tools:

- **`demarkus`** (CLI): fetch/list/publish/versions + graph crawl
- **`demarkus-tui`** (TUI): interactive markdown browser
- **`demarkus-mcp`** (MCP server): tools for LLM agents
- **`demarkus-agent`** (federation crawler): scheduled crawl, hash indexes, graph snapshots

All tools share the same Mark Protocol client layer with:

- Connection pooling
- Retry on transient errors
- Optional response caching (etag / if-modified-since)

## Wire Format (Request / Response)

### Request

```
FETCH /hello.md
```

### Response

```
---
status: ok
modified: 2025-02-14T10:30:00Z
version: 1
---

# Hello World
```

Status codes are **text strings**, not numeric.

## Versioning & Integrity

Every write creates a **new immutable version**. The server stores versions in a `versions/` directory and links them by a SHA‑256 hash chain.

```
root/
  doc.md              ← symlink → versions/doc.md.v3
  versions/
    doc.md.v1         ← genesis
    doc.md.v2         ← sha256 of v1 bytes
    doc.md.v3         ← sha256 of v2 bytes
```

The server can verify chain integrity and expose it via `VERSIONS`.

## Authentication Model

Authentication is capability‑based:

- Tokens grant **operations on path patterns**
- Tokens are stored **hashed** (SHA‑256) in a TOML file
- No accounts, no identities, no sessions

If no tokens file is configured, the server is **read‑only** (secure by default).

## Security Decisions

- **Encrypted transport** (TLS over QUIC)
- **Path traversal protection** (clean + explicit `..` check)
- **No client‑side execution**
- **Minimal logging** (no tracking)

## Design Principles in Practice

| Principle | Implementation |
|----------|----------------|
| Simplicity | Small, composable modules |
| Privacy | No tracking or analytics |
| Integrity | Immutable versions with hash chain |
| Federation | Anyone can run a server |
| Clarity | Text status codes, human‑readable protocol |

## Document Graph

Markdown links form a natural **document graph**. The CLI, TUI, and MCP server include graph crawling features to visualize and verify link structure.

The client-side `graphstore` package persists crawled graph data at `~/.mark/graph.json`. All three tools share this store — a graph built by the CLI is visible in the TUI and queryable via MCP. Each crawl merges new nodes and edges into the existing graph, so knowledge accumulates across sessions.

Backlinks ("what documents link here?") are derived from the stored graph with no server-side changes needed. The MCP `mark_backlinks` tool exposes this as a query for agents. The store is seeded on demand from the world's atomic `/graph/manifest.md` snapshot, with `/graph.md` as a compatibility fallback, so a fresh client answers backlinks before its first crawl; local crawls take precedence over seeded rows.

The graph itself is exportable as a publishable markdown document. `mark_graph_export` renders the graph; `mark_graph_publish` exports and publishes the legacy single-document form in one step. The federation agent additionally publishes version-pinned node and edge shards behind a manifest-last CAS, allowing clients to import a complete generation without recrawling original documents.

## Open Knowledge Format (OKF) Compatibility

Demarkus aligns its document content model with [Google's Open Knowledge Format v0.1](https://github.com/GoogleCloudPlatform/knowledge-catalog). The compatibility is deliberately scoped to two layers:

- **Document level — compatible.** A stored document's persisted metadata uses OKF field names for the recognized fields (`type`, `title`, `description`, `resource`, `tags`, `timestamp`), with non-spec keys namespaced under `meta.*` and store-operational fields (`version`, `previous-hash`, `archived`) reserved by name. The server assigns a default `type: Document` on PUBLISH and APPEND when none is declared, so every concept is OKF-typed by construction.
- **System level — superset.** Demarkus layers immutable versioning, a SHA-256 hash chain, QUIC transport, capability-based auth, and LOOKUP discovery on top of an OKF-compatible document. It is *not* an OKF bundle server: frontmatter is stripped before serving and the `versions/` layout is not a bundle tree.

Bundle interoperability is therefore an out-of-band codec, not a wire feature. The `demarkus okf` subcommand (`client/internal/okf`) round-trips bundles — `validate` (v0.1 conformance), `import` (bundle → world), `export` (world subtree → conformant bundle) — with verified byte-identical body round-trips. See [SPEC §14](../../SPEC.md) and ADRs 0002 (metadata alignment) and 0003 (default type).

## Deployment Topology

Demarkus deploys at three scales.

### Single server

Clients connect over QUIC to one `demarkus-server`; the content store is the filesystem (versioned files under `versions/`) or Postgres (`demarkus-server-pg`).

```
Clients (CLI/TUI/MCP)
        |
     QUIC/TLS
        |
  demarkus-server
        |
   /srv/site (files + versions/)   or   Postgres
```

The filesystem form needs no database and no background services.

### Brokered knowledge system

`demarkus-broker` fronts one or more worlds: it terminates MCP over HTTPS with OIDC auth, mints per-world capability tokens, and speaks QUIC to the worlds behind it. The federation agent (`demarkus-agent`) crawls the worlds on a schedule and publishes hash indexes and graph snapshots to the hub.

```
Agents (MCP over HTTPS, OIDC)
        |
  demarkus-broker ── demarkus-agent (scheduled crawl)
        |
   QUIC/TLS (per-world tokens)
        |
  worlds (demarkus-server instances)
```

### Multi-world knowledge server

At production scale, one `demarkus-knowledge-server` deployment replaces per-world server processes: TLS SNI selects the world during the QUIC handshake, each world has its own GCS bucket with an immutable world ID, and 2 or more stateless replicas share the buckets with no leader election (writes race on a compare-and-swap of one head object). See [Kubernetes & Helm](../deployment/kubernetes.md).

## Request Flow

A single request follows a simple path:

1. Client opens a QUIC stream and sends a request line (e.g. `FETCH /index.md`).
2. Server parses the request and validates the path.
3. Server reads content from the store (or writes a new version for `PUBLISH`).
4. Server returns a response with YAML frontmatter + markdown body.
5. Client renders the response (or caches it if enabled).

Because each request is independent and the protocol is text‑based, the system is easy to debug and test.

## Related

- [Philosophy & Intent](../philosophy/index.md)
- [Protocol Specification](../../SPEC.md)
- [Docs Home](../index.md)
