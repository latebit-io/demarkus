---
layout: default
title: The Mark Protocol
permalink: /about/
nav: docs
---

# The Mark Protocol

Memory and knowledge sit on one foundation: versioned markdown served over QUIC.
You never need to touch it to use Demarkus. It is why your notes stay portable
and self-hostable.

- **Versioned**: every write is a new immutable version, hash-chained. History is kept unless pruned with retention.
- **Read-only by default**: writes need a capability token or a signed-in identity.
- **Private**: no tracking, no cookies, no user agents logged.
- **Encrypted**: QUIC with TLS 1.3, always.
- **Markdown only**: no scripts, no templates, nothing executes.
- **Self-hostable**: one static binary, no dependencies.
- **Open**: CC0 specification, AGPL-3.0 implementation. Anyone can run a server.

## At a glance

**Transport**: QUIC, UDP 6309 · **Scheme**: `mark://` · **Content**: markdown + YAML frontmatter

```text
FETCH /hello.md
```

```text
---
status: ok
version: 3
modified: 2026-01-15T10:30:00Z
---

# Hello World
```

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `LOOKUP` · `WATCH` (read) · `PUBLISH` · `APPEND` · `ARCHIVE` (write)

## Portable by design

Every reader is a potential mirror. Content is cached when you read it, and you
can re-host it from the cache. Documents are content-compatible with the
[Open Knowledge Format](/reference/markdown/#open-knowledge-format-okf-compatibility)
v0.1; `demarkus okf` validates, imports, and exports bundles.

## Read more

- [Protocol specification](https://github.com/latebit-io/demarkus/blob/main/docs/SPEC.md)
- [Design rationale](https://github.com/latebit-io/demarkus/blob/main/docs/DESIGN.md)
- [Security model](/security/)
