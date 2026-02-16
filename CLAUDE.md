# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development Philosophy

**Small, incremental changes.** Robustness and stability are the highest priorities. Every change should be minimal, tested, and working before moving to the next step. Do not combine unrelated changes. Do not skip tests. Do not rush ahead — build on solid ground.

**Concise, readable code with minimal cognitive load.** Favour the simplest solution that works. Code must be highly maintainable by both humans and agents — short functions, clear names, obvious flow. Avoid cleverness, unnecessary abstraction, and deep nesting. If a reader has to pause to understand what a block does, simplify it.

## Build & Test Commands

```bash
make all                # Build protocol, server, and client
make server             # Build server only (depends on protocol)
make client             # Build client only (depends on protocol)
make test               # Run all tests across all modules
make fmt                # Format all code
make vet                # Vet all code
make deps               # go mod tidy + download for all modules
make clean              # Remove build artifacts

# Run tests for a single module
cd protocol && go test ./...
cd server && go test ./...

# Run a single test
cd protocol && go test -run TestParseRequest ./...
cd server && go test -run TestHandleFetch/path_traversal_blocked ./internal/handler/

# Run server for development
./server/bin/demarkus-server -root ./examples/demo-site

# Run client
./client/bin/demarkus mark://localhost:6309/index.md
./client/bin/demarkus -X LIST mark://localhost:6309/
```

## Architecture

This is a Go monorepo implementing the **Mark Protocol** — a QUIC-based, markdown-native document protocol. Four independent Go modules with local `replace` directives:

```
protocol/  → shared wire format types (foundation, no network code)
server/    → QUIC server serving .md files (depends on protocol)
client/    → CLI client fetching documents (depends on protocol)
tools/     → dev utilities (placeholder)
```

**Protocol** is the pure parsing/serialization layer. It knows nothing about QUIC, TLS, or filesystems. Server and client both depend on it.

### Wire Format

Request: newline-terminated text line (`FETCH /path.md\n`)

Response: YAML frontmatter + markdown body:
```
---
status: ok
modified: 2025-02-14T10:30:00Z
version: 1
---
# Content here
```

Status values are text-based strings (`ok`, `not-found`, `server-error`), not numeric codes.

### Key Design Decisions

- **`handler.Stream` interface** (`io.ReadWriteCloser`): decouples handler from QUIC, enabling fast unit tests with mock streams (no network needed)
- **YAML frontmatter parsed as `map[string]string`**: avoids YAML auto-typing timestamps/numbers into Go types
- **Server strips file frontmatter and re-emits protocol frontmatter**: preserves `version` from files, adds `modified` from filesystem mtime
- **Ed25519 self-signed TLS certs generated in-memory** for dev (`server/internal/tls/`). Client uses `InsecureSkipVerify` in dev mode
- **Path traversal protection**: `filepath.Clean` + explicit `..` check, returns `not-found` (not `forbidden`) to avoid info disclosure

### Protocol Constants

- Default port: `6309`
- ALPN identifier: `"mark"`
- URL scheme: `mark://`

## Testing Patterns

- **Table-driven tests** with `t.Run` subtests throughout
- **Round-trip tests**: serialize → parse → compare to verify wire format correctness
- **Mock stream** in handler tests: `mockStream` struct with `io.Reader` + `bytes.Buffer` output, no QUIC required
- **`t.TempDir()`** for filesystem test fixtures with automatic cleanup

## CI/CD & Releases

Each module is versioned and released independently using **conventional commits**.

**Commit format** — scope determines which module gets a version bump:
```
feat(server): add config file support     → server minor bump
fix(client): handle connection timeout     → client patch bump
feat(protocol): add WRITE verb             → protocol minor bump
feat!: breaking change                     → major bump
```

**Pipeline**: push to main → CI tests changed modules → auto-release scans conventional commits per module → creates tags → GoReleaser builds binaries, Docker images, and creates GitHub releases. No PRs, fully automatic.

**Tags** use Go module-compatible prefixes: `server/v0.1.0`, `client/v0.1.0`, `protocol/v0.1.0`.

**Key files**: `.github/workflows/auto-release.yml`, `server/.goreleaser.yml`, `client/.goreleaser.yml`.

Protocol is a library (tag-only, no binary). Server and client produce GoReleaser builds. Server also pushes Docker images to `ghcr.io/latebit-io/demarkus-server`.

While in `0.x.x`, breaking changes bump minor (not major). Major `1.0.0` will be an explicit decision.

## Core Protocol Invariants

**Version immutability is vital.** Every write to a document creates a new version. Published versions are permanent and must never be modified or deleted. Version history is an append-only log — this enables distributed verification, censorship resistance, and prevents historical revisionism. If content needs correction, publish a new version.

**Security is foundational.** No tracking, no telemetry, no client-side execution. Transport is always encrypted. Paths are always validated. Auth grants capabilities, not identities.

## Current State

Phase 1 MVP (read-only). `FETCH` and `LIST` verbs are implemented. Server uses env-based config (`DEMARKUS_` prefix) with flag overrides for dev. No auth, no versioning, no caching, no TUI — just QUIC transport serving markdown files end-to-end. See `docs/DESIGN.md` for the full protocol specification and roadmap.
