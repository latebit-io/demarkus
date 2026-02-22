# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus reimagines the web around markdown — a format structured and optimized for machines, familiar and loved by humans. Built for a world where humans and AI agents read and write together, it delivers content directly over QUIC: no rendering pipeline, no tracking, no commercialization, no unnecessary complexity. Privacy and security are foundational. 

## Project Status

🟡 **Phase 2 — Read/Write MVP** — `FETCH`, `LIST`, `VERSIONS`, and `PUBLISH` are all working. Auth, caching, TUI browser, MCP server, and link-graph crawler are implemented.

## Quick Links

- **Protocol Specification**: [docs/SPEC.md](docs/SPEC.md)
- **Design Document**: [docs/DESIGN.md](docs/DESIGN.md)
- **User Guide**: [docs/USER-GUIDE.md](docs/USER-GUIDE.md)

## Components

### Protocol (`protocol/`)
Pure Go library implementing the Mark Protocol wire format. No network code, no filesystem — just parsing and serialization.
- Request parsing (`FETCH /path.md\n`)
- Response parsing/encoding (YAML frontmatter + markdown body)
- Shared constants (port 6309, ALPN `mark`, verb names, status values)

### Server (`server/`)
Reference QUIC server for the Mark Protocol.
- Serves markdown files over QUIC with TLS
- Versioned document store — every `PUBLISH` creates an immutable version linked by a SHA-256 hash chain
- Capability-based auth: tokens grant operations on path patterns, stored as SHA-256 hashes (never plaintext)
- Conditional responses: `etag` / `if-none-match` and `if-modified-since` support
- Path traversal protection and 10 MB file-size limit
- Self-signed dev cert generated in-memory; production TLS loaded from disk
- Hot certificate reload on `SIGHUP` (no connection drop)
- Graceful shutdown on `SIGINT`/`SIGTERM`
- Health check: `FETCH /health`

### Client (`client/`)
Three client tools sharing the same connection and fetch layer:

**`demarkus`** — CLI for scripting and automation:
- `FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, and `graph` subcommand
- Response caching with `etag`/`if-modified-since` revalidation
- Connection pool with automatic retry on transient errors
- Auth token via `-auth` flag or `DEMARKUS_AUTH` env var

**`demarkus-tui`** — Terminal browser with keyboard navigation:
- Markdown rendered with Glamour
- Address bar, scrollable viewport, status bar
- Link navigation with `Tab`, back/forward history with `[`/`]`
- Document graph view (`d`) — crawls outbound links and displays a tree
- Mouse support (click to focus, scroll wheel)

**`demarkus-mcp`** — MCP server (Model Context Protocol) for LLM agents:
- Exposes `mark_fetch`, `mark_list`, `mark_graph`, and `mark_publish` tools
- Stdio transport, compatible with Claude Desktop and any MCP client
- Optional `-host` flag for connecting to a single server without full `mark://` URLs

### Tools (`tools/`)
Development utilities placeholder.

## Getting Started

### Prerequisites

- Go 1.26 or later
- Make (optional)

### Building

```bash
# Build everything
make all

# Or build individually
make server    # → server/bin/demarkus-server, server/bin/demarkus-token
make client    # → client/bin/demarkus, client/bin/demarkus-tui, client/bin/demarkus-mcp
```

### Running

**Start a server (dev mode — self-signed cert)**:
```bash
./server/bin/demarkus-server -root ./examples/demo-site
```

**CLI — fetch a document**:
```bash
./client/bin/demarkus --insecure mark://localhost:6309/index.md
```

**CLI — list a directory**:
```bash
./client/bin/demarkus --insecure -X LIST mark://localhost:6309/
```

**CLI — fetch a specific version**:
```bash
./client/bin/demarkus --insecure mark://localhost:6309/doc.md/v2
```

**CLI — view version history**:
```bash
./client/bin/demarkus --insecure -X VERSIONS mark://localhost:6309/doc.md
```

**CLI — crawl the document graph**:
```bash
./client/bin/demarkus --insecure graph -depth 3 mark://localhost:6309/index.md
```

**TUI browser**:
```bash
./client/bin/demarkus-tui --insecure mark://localhost:6309/index.md
```

**MCP server** (attach to a specific host):
```bash
./client/bin/demarkus-mcp -host mark://localhost:6309 -insecure
```

## TUI Keyboard Reference

| Key | Action |
|-----|--------|
| `Enter` | Follow selected link / fetch URL in address bar |
| `Tab` | Cycle through links on page |
| `[` / `Alt+Left` | Go back |
| `]` / `Alt+Right` | Go forward |
| `d` | Toggle document graph view |
| `f` | Focus address bar |
| `j` / `↓` | Scroll down |
| `k` / `↑` | Scroll up |
| `g` | Go to top |
| `G` | Go to bottom |
| `?` | Toggle help screen |
| `q` / `Ctrl+C` | Quit |

## Setting Up Authentication

The server is **secure by default** — writes are denied unless you configure a tokens file. Tokens are capability-based: they grant specific operations on specific paths, not identities.

**1. Generate a token**:
```bash
# Grant publish access to all paths
./server/bin/demarkus-token generate -paths "/*" -ops publish -tokens tokens.toml
```

This prints the raw token (shown once; give it to the client) and appends the hashed entry to `tokens.toml`. The server never stores the raw token — only its SHA-256 hash.

**2. Start the server with auth**:
```bash
./server/bin/demarkus-server -root /srv/site -tokens /path/to/tokens.toml
```

Or via environment variable:
```bash
export DEMARKUS_TOKENS=/etc/demarkus/tokens.toml
./server/bin/demarkus-server -root /srv/site
```

**3. Write content through the protocol**:
```bash
./client/bin/demarkus --insecure -X PUBLISH -auth <raw-token> mark://localhost:6309/hello.md -body "# Hello World"
```

You can also set the token via environment variable:
```bash
export DEMARKUS_AUTH=<raw-token>
./client/bin/demarkus --insecure -X PUBLISH mark://localhost:6309/hello.md -body "# Hello World"
```

**Token scoping examples**:
```bash
# Publish-only to /docs/*
./server/bin/demarkus-token generate -paths "/docs/*" -ops publish -tokens tokens.toml

# Read and publish to everything
./server/bin/demarkus-token generate -paths "/*" -ops "read,publish" -tokens tokens.toml
```

## Adding Content

All content must be published through the protocol. **Files copied directly to the filesystem are not served** — only documents with proper version history (published via `PUBLISH`) are accessible. This ensures every document has an immutable version chain and tamper detection.

```bash
# Publish a new document (creates version 1)
./client/bin/demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/about.md -body "# About\n\nWelcome."

# Update it (creates version 2, linked by hash chain)
./client/bin/demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/about.md -body "# About\n\nUpdated content."

# Verify the version history
./client/bin/demarkus --insecure -X VERSIONS mark://localhost:6309/about.md

# Fetch a specific historical version
./client/bin/demarkus --insecure mark://localhost:6309/about.md/v1
```

## Response Caching

The CLI and TUI automatically cache responses in `~/.mark/cache/`. On repeated requests the client sends `if-none-match` (etag) and `if-modified-since` headers; the server replies with `not-modified` when content hasn't changed, and the cached copy is served instantly.

```bash
# Disable caching for a single request
./client/bin/demarkus --insecure --no-cache mark://localhost:6309/index.md

# Override cache directory
./client/bin/demarkus --insecure --cache-dir /tmp/mark-cache mark://localhost:6309/index.md
# Or: export DEMARKUS_CACHE_DIR=/tmp/mark-cache
```

## Deploying with Let's Encrypt

The server supports loading TLS certificates from disk for production deployments.

**1. Install certbot and obtain a certificate**:
```bash
# Install certbot (Ubuntu/Debian)
sudo apt install certbot

# Obtain a certificate (standalone mode, requires port 80 open temporarily)
sudo certbot certonly --standalone -d yourdomain.com
```

Certificates are saved to `/etc/letsencrypt/live/yourdomain.com/`.

**2. Start the server with real certificates**:
```bash
./demarkus-server \
  -root /srv/blog \
  -tls-cert /etc/letsencrypt/live/yourdomain.com/fullchain.pem \
  -tls-key /etc/letsencrypt/live/yourdomain.com/privkey.pem
```

Or using environment variables:
```bash
export DEMARKUS_ROOT=/srv/blog
export DEMARKUS_TLS_CERT=/etc/letsencrypt/live/yourdomain.com/fullchain.pem
export DEMARKUS_TLS_KEY=/etc/letsencrypt/live/yourdomain.com/privkey.pem
./demarkus-server
```

**3. Open the QUIC port** (UDP 6309):
```bash
sudo ufw allow 6309/udp
```

**4. Connect from a client**:
```bash
./demarkus mark://yourdomain.com/index.md
```

**5. Auto-renew certificates** with a cron job or systemd timer:
```bash
# Runs twice daily; reloads cert on renewal — no downtime
0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"
```

The server reloads certificates on `SIGHUP` without dropping connections. If you prefer a full restart:
```bash
0 */12 * * * certbot renew --quiet --deploy-hook "systemctl restart demarkus"
```

**Example systemd service** (`/etc/systemd/system/demarkus.service`):
```ini
[Unit]
Description=Demarkus Mark Protocol Server
After=network.target

[Service]
Type=simple
User=demarkus
ExecStart=/usr/local/bin/demarkus-server
Environment=DEMARKUS_ROOT=/srv/blog
Environment=DEMARKUS_TLS_CERT=/etc/letsencrypt/live/yourdomain.com/fullchain.pem
Environment=DEMARKUS_TLS_KEY=/etc/letsencrypt/live/yourdomain.com/privkey.pem
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now demarkus
```

## Server Configuration Reference

All settings are via environment variables; flags override for dev use:

| Env var | Flag | Default | Description |
|---------|------|---------|-------------|
| `DEMARKUS_ROOT` | `-root` | *(required)* | Content directory to serve |
| `DEMARKUS_PORT` | `-port` | `6309` | UDP port to listen on |
| `DEMARKUS_TLS_CERT` | `-tls-cert` | *(dev cert)* | Path to TLS certificate PEM |
| `DEMARKUS_TLS_KEY` | `-tls-key` | *(dev cert)* | Path to TLS private key PEM |
| `DEMARKUS_TOKENS` | `-tokens` | *(none — writes disabled)* | Path to TOML tokens file |
| `DEMARKUS_MAX_STREAMS` | — | `10` | Max concurrent streams per connection |
| `DEMARKUS_IDLE_TIMEOUT` | — | `30s` | Idle connection timeout |
| `DEMARKUS_REQUEST_TIMEOUT` | — | `10s` | Per-request deadline |

## Protocol Overview

**Transport**: QUIC (UDP port 6309)  
**Scheme**: `mark://`  
**Content**: Markdown with YAML frontmatter

**Implemented verbs**:
- `FETCH` — Retrieve
 a document (or a specific version via `/path/vN`)
- `LIST` — Directory contents
- `VERSIONS` — Full version history with hash-chain validity
- `PUBLISH` — Create or update a document (requires auth token)

**Planned verbs**:
- `APPEND` — Add content to a document
- `ARCHIVE` — Remove a document from serving
- `SEARCH` — Full-text search

**Example request**:
```
FETCH /hello.md
```

**Example response**:
```
---
status: ok
modified: 2025-02-14T10:30:00Z
version: 1
---

# Hello World

Welcome to Demarkus!
```

**Status values** (text strings, not numeric codes):  
`ok` · `created` · `not-modified` · `not-found` · `unauthorized` · `not-permitted` · `server-error`

## Version Integrity (Hash Chain)

Every document write creates a new immutable version. Versions are linked by a hash chain that guarantees tamper detection.

**On-disk layout**:
```
root/
  doc.md              ← symlink → versions/doc.md.v3
  versions/
    doc.md.v1         ← genesis (no previous-hash)
    doc.md.v2         ← sha256 of doc.md.v1 raw bytes
    doc.md.v3         ← sha256 of doc.md.v2 raw bytes
```

**Each version file** includes store-managed frontmatter:
```
---
version: 2
previous-hash: sha256-a1b2c3d4e5f6...
---
# Document content
```

The server verifies the chain when `VERSIONS` is called — computing `sha256(raw bytes of vN-1)` and comparing it against `previous-hash` in `vN`. Any tampered version breaks the chain and is reported.

```
versions/doc.md.v1          versions/doc.md.v2          versions/doc.md.v3
┌──────────────────┐        ┌──────────────────┐        ┌──────────────────┐
│ version: 1       │        │ version: 2       │        │ version: 3       │
│ (genesis)        │  ──►   │ previous-hash:   │  ──►   │ previous-hash:   │
│ # Hello          │  hash  │   sha256-a1b2... │  hash  │   sha256-f6e5... │
│                  │  of    │ # Updated Hello  │  of    │ # Third revision │
└──────────────────┘  file  └──────────────────┘  file  └──────────────────┘
```

This gives the same guarantees as a git commit chain — any modification to any historical version is detectable.

## Core Principles

1. **Optimized for Information**: Markdown is the common language — structured enough for agents, readable enough for humans
2. **Privacy First**: No user tracking, minimal logging, anonymity by default
3. **Security Minded**: Encryption mandatory, capability-based auth, secure by default
4. **Simplicity**: Human-readable protocol, minimal complexity
5. **Anti-Commercialization**: No ads, no tracking, no central authority
6. **Federation**: Anyone can run a server, content can be mirrored freely

## Philosophy

Demarkus embodies the **library model** rather than the **platform model**:
- Anyone can copy documents (like books)
- Anyone can run a server (like a library)
- Knowledge wants to be free
- Preservation over profit

Content persists through distributed caching — every client is a potential mirror. This creates natural censorship resistance without requiring complex distributed systems.

## Contributing

Early-stage development. The protocol specification is still evolving. Contributions, feedback, and critiques are welcome!

## License

- Protocol Specification: CC0 (Public Domain)
- Implementation Code: MIT (TBD)

## Links

- **Specification**: [docs/SPEC.md](docs/SPEC.md)
- **Design Rationale**: [docs/DESIGN.md](docs/DESIGN.md)
- **User Guide**: [docs/USER-GUIDE.md](docs/USER-GUIDE.md)

---

*"The web we want, not the web we got."*
