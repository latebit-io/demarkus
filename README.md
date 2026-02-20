# Demarkus

**A privacy-first, markdown-native web protocol**

Demarkus reimagines the web around markdown, with privacy and security as foundational principles. Built on QUIC, it provides a lightweight, human-readable protocol for document-centric communication free from tracking, commercialization, and unnecessary complexity.

## Project Status

🚧 **Early Development** - MVP in progress

## Quick Links

- **Protocol Specification**: [docs/SPEC.md](docs/SPEC.md)
- **Design Document**: [docs/DESIGN.md](docs/DESIGN.md)

## Components

### Protocol (`protocol/`)
Core Go library implementing the Mark Protocol specification.
- Message parsing (FETCH, WRITE, APPEND, etc.)
- QUIC transport layer
- Authentication & capability system
- Version management

### Server (`server/`)
Reference implementation of a Demarkus server.
- Serves markdown files over QUIC
- File-based storage with versioning
- Capability-based authentication
- Privacy-focused logging

### Client (`client/`)
Terminal-based browser for the Mark Protocol.
- TUI interface built with Bubble Tea
- Markdown rendering with Glamour
- Local caching for offline reading
- Navigation history

### Tools (`tools/`)
Development and testing utilities.

## Getting Started

### Prerequisites

- Go 1.26 or later
- Make (optional)

### Building

```bash
# Build everything
make all

# Or build individually
cd protocol && go build ./...
cd server && go build -o bin/demarkus-server ./cmd/demarkus-server
cd client && go build -o bin/demarkus ./cmd/demarkus
```

### Running

**Start a server (dev mode, read-only)**:
```bash
./server/bin/demarkus-server -root ./examples/demo-site
```

**Run the client**:
```bash
./client/bin/demarkus --insecure mark://localhost:6309/index.md
```

**List directory contents**:
```bash
./client/bin/demarkus --insecure -X LIST mark://localhost:6309/
```

### Setting Up Authentication

The server is **secure by default** — writes are denied unless you configure a tokens file. Tokens are capability-based: they grant specific operations on specific paths, not identities.

**1. Generate a token**:
```bash
# Generate a token with write access to all paths
./server/bin/demarkus-token generate -paths "/*" -ops write -tokens tokens.toml
```

This prints the raw token (give to the client, shown once) and appends the hashed entry to `tokens.toml`. The server never stores the raw token — only its SHA-256 hash.

**2. Start the server with auth**:
```bash
./server/bin/demarkus-server -root /srv/site -tokens tokens.toml
```

**3. Write content through the protocol**:
```bash
./client/bin/demarkus --insecure -X WRITE -auth <raw-token> mark://localhost:6309/hello.md -body "# Hello World"
```

You can also set the token via environment variable:
```bash
export DEMARKUS_AUTH=<raw-token>
./client/bin/demarkus --insecure -X WRITE mark://localhost:6309/hello.md -body "# Hello World"
```

**Token scoping examples**:
```bash
# Write-only to /docs/*
./server/bin/demarkus-token generate -paths "/docs/*" -ops write -tokens tokens.toml

# Read and write to everything
./server/bin/demarkus-token generate -paths "/*" -ops "read,write" -tokens tokens.toml
```

### Adding Content

All content must be written through the protocol. **Files copied directly to the filesystem are not served** — only documents with proper version history (written via WRITE) are accessible. This ensures every document has an immutable version chain and tamper detection.

```bash
# Write a new document (creates version 1)
./client/bin/demarkus --insecure -X WRITE -auth $TOKEN mark://localhost:6309/about.md -body "# About\n\nWelcome."

# Update it (creates version 2, linked by hash chain)
./client/bin/demarkus --insecure -X WRITE -auth $TOKEN mark://localhost:6309/about.md -body "# About\n\nUpdated content."

# Verify the version history
./client/bin/demarkus --insecure -X VERSIONS mark://localhost:6309/about.md
```

### Deploying with Let's Encrypt

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
  -tls-cert /etc/letsencrypt/live/demarkus.latebit.io/fullchain.pem \
  -tls-key /etc/letsencrypt/live/demarkus.latebit.io/privkey.pem
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
# Add to crontab (runs twice daily, reloads cert on renewal — no downtime)
0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"
```

The server reloads certificates on `SIGHUP` without dropping connections. If you prefer a full restart instead:
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

## Core Principles

1. **Privacy First**: No user tracking, minimal logging, anonymity by default
2. **Security Minded**: Encryption mandatory, capability-based auth
3. **Simplicity**: Human-readable protocol, minimal complexity
4. **Anti-Commercialization**: No ads, no tracking, no central authority
5. **Federation**: Anyone can run a server, content can be mirrored freely

## Protocol Overview

**Transport**: QUIC (port 6309)  
**Scheme**: `mark://`  
**Content**: Markdown with YAML frontmatter  

**Verbs**:
- `FETCH` - Retrieve documents
- `WRITE` - Create/update documents (requires auth token)
- `LIST` - Directory contents
- `VERSIONS` - Version history
- `APPEND` - Add content (future)
- `ARCHIVE` - Remove from serving (future)
- `SEARCH` - Find documents (future)

**Example Request**:
```
FETCH /hello.md
```

**Example Response**:
```markdown
---
status: ok
modified: 2025-02-14T10:30:00Z
version: 1
---

# Hello World

Welcome to Demarkus!
```

## Version Integrity (Hash Chain)

Every document write creates a new immutable version. Versions are linked by a hash chain that guarantees tamper detection.

**On-disk layout**:
```
root/
  doc.md              ← symlink to versions/doc.md.v3
  versions/
    doc.md.v1         ← genesis (no previous-hash)
    doc.md.v2         ← contains sha256 of v1's raw bytes
    doc.md.v3         ← contains sha256 of v2's raw bytes
```

**Each version file** includes store-managed frontmatter:
```markdown
---
version: 2
previous-hash: sha256-a1b2c3d4e5f6...
---
# Document content
```

**Verification**: the `VERSIONS` response includes a `chain-valid` metadata field. The server walks the chain from oldest to newest — for each version, it computes `sha256(raw bytes of vN-1)` and compares it against the `previous-hash` recorded in `vN`. If any version has been modified, the hash won't match and the chain is reported as broken:

```
versions/doc.md.v1          versions/doc.md.v2          versions/doc.md.v3
┌──────────────────┐        ┌──────────────────┐        ┌──────────────────┐
│ ---              │        │ ---              │        │ ---              │
│ version: 1       │        │ version: 2       │        │ version: 3       │
│ ---              │   ──►  │ previous-hash:   │   ──►  │ previous-hash:   │
│ # Hello          │  hash  │   sha256-a1b2... │  hash  │   sha256-f6e5... │
│                  │  of    │ ---              │  of    │ ---              │
│                  │  this  │ # Updated Hello  │  this  │ # Third revision │
└──────────────────┘  file  └──────────────────┘  file  └──────────────────┘
```

This gives the same guarantees as a git commit chain — you can always detect if any version in the history has been altered.

## Philosophy

Demarkus embodies the **library model** rather than the **platform model**:
- Anyone can copy documents (like books)
- Anyone can run a server (like a library)
- Knowledge wants to be free
- Preservation over profit

Content persists through distributed caching - every client is a potential mirror. This creates natural censorship resistance without requiring complex distributed systems.

## Contributing

This is early-stage development. The protocol specification is still evolving. Contributions, feedback, and critiques are welcome!

## License

- Protocol Specification: CC0 (Public Domain)
- Implementation Code: MIT (TBD)

## Links

- **Website**: [To be determined]
- **Specification**: [docs/SPEC.md](docs/SPEC.md)
- **Design Rationale**: [docs/DESIGN.md](docs/DESIGN.md)

---

*"The web we want, not the web we got."*
