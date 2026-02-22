# Architecture & Design

This page describes the Demarkus system architecture, key design decisions, and how the protocol, server, and clients fit together. It is intended as a practical overview for operators, contributors, and integrators.

## System Overview

Demarkus is a markdown‑native document protocol built on QUIC. It is organized as four modules:

- **`protocol/`** — wire format and parsing/serialization only
- **`server/`** — QUIC server serving versioned markdown files
- **`client/`** — CLI, TUI, and MCP tools
- **`tools/`** — development utilities (placeholder)

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
- **Store**: versioned document storage with hash‑chain integrity
- **Auth**: capability‑based token verification (hashes only)
- **TLS**: dev cert generation, prod cert loading, SIGHUP reload

### Clients

The client module ships three tools:

- **`demarkus`** (CLI): fetch/list/publish/versions + graph crawl
- **`demarkus-tui`** (TUI): interactive markdown browser
- **`demarkus-mcp`** (MCP server): tools for LLM agents

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

Markdown links form a natural **document graph**. The CLI and TUI include graph crawling features to visualize and verify link structure.

## Deployment Topology

A typical deployment looks like this:

- **Clients** (CLI/TUI/MCP) connect over QUIC to a server.
- **Server** terminates TLS, validates paths, and serves content from a local directory.
- **Content store** is just the filesystem; versioned files live under `versions/`.

Example topology:

```
Clients (CLI/TUI/MCP)
        |
     QUIC/TLS
        |
  demarkus-server
        |
   /srv/site (files + versions/)
```

This design keeps infrastructure minimal: no database, no external dependencies, and no background workers.

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
- [Protocol Specification](../../spec.md)
- [Docs Home](../index.md)