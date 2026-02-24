# CLAUDE.md

## Code Style

Use `range N` for integer loops (not `for i := 0; i < N; i++`).

## Development Philosophy

Small, incremental changes. Robustness first. Minimal, tested, working before moving on. Favour the simplest solution — short functions, clear names, obvious flow.

## Build & Test

```bash
make all        # Build everything
make test       # Run all tests
make fmt        # Format
make vet        # Vet

# Single module / single test
cd server && go test -run TestHandleFetch/path_traversal_blocked ./internal/handler/

# Dev server
./server/bin/demarkus-server -root ./examples/demo-site
```

**Build rule**: Always use `make client` or `go build -o bin/<name> ./cmd/<name>/`. Never bare `go build ./cmd/<name>/`.

## Architecture

Go monorepo, four modules with local `replace` directives:

- `protocol/` — wire format types, parsing, serialization (no network code)
- `server/` — QUIC server (depends on protocol)
- `client/` — CLI, TUI, MCP server (depends on protocol)
- `tools/` — dev utilities

**Wire format**: Request is `VERB /path\n` with optional YAML frontmatter + body. Response is YAML frontmatter + markdown body. Status values are text strings (`ok`, `not-found`, etc.), not numeric codes.

**Protocol constants**: port `6309`, ALPN `"mark"`, scheme `mark://`

## Key Design Decisions

- `handler.Stream` = `io.ReadWriteCloser` (mock streams for tests, no QUIC needed)
- Frontmatter parsed as `map[string]string` (avoids YAML auto-typing)
- Versioned store with symlinks: `doc.md` → `versions/doc.md.v<N>`, SHA-256 hash chain
- Capability-based auth: hashed tokens in TOML, per-path glob, per-operation grants
- Path traversal: `filepath.Clean` + `..` check, returns `not-found` (not `forbidden`)

## Testing Patterns

Table-driven tests with `t.Run`. Mock streams for handler tests. `t.TempDir()` for fixtures.

## CI/CD

Conventional commits with module scope determine version bumps:
```
feat(server): description  → server minor bump
fix(client): description   → client patch bump
```
Tags: `server/v0.1.0`, `client/v0.1.0`, `protocol/v0.1.0`. Push to main triggers auto-release.

## Core Invariants

- **Version immutability**: every write creates a new version, published versions are permanent
- **Security**: no tracking, no telemetry, encrypted transport, capability-based auth

## Roadmap

See `docs/DESIGN.md` for full spec and roadmap.
