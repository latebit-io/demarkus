# Demarkus

**A privacy-first, markdown-native web protocol**

Demarkus reimagines the web around markdown, with privacy and security as foundational principles. Built on QUIC, it provides a lightweight, human-readable protocol for document-centric communication free from tracking, commercialization, and unnecessary complexity.

## Project Status

🚧 **Early Development** - MVP in progress

## Quick Links

- **Protocol Specification**: [docs/protocol-spec.md](docs/protocol-spec.md)
- **Design Document**: [docs/DESIGN.md](docs/DESIGN.md)
- **Server Documentation**: [server/README.md](server/README.md)
- **Client Documentation**: [client/README.md](client/README.md)

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

**Start a server (dev mode)**:
```bash
./server/bin/demarkus-server -root ./examples/demo-site
```

**Run the client**:
```bash
./client/bin/demarkus -insecure mark://localhost:6309/index.md
```

**List directory contents**:
```bash
./client/bin/demarkus -insecure -X LIST mark://localhost:6309/
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
  -root /srv/content \
  -tls-cert /etc/letsencrypt/live/yourdomain.com/fullchain.pem \
  -tls-key /etc/letsencrypt/live/yourdomain.com/privkey.pem
```

Or using environment variables:
```bash
export DEMARKUS_ROOT=/srv/content
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
0 */12 * * * certbot renew --quiet --deploy-hook "kill -HUP $(pidof demarkus-server)"
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
Environment=DEMARKUS_ROOT=/srv/content
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
- `WRITE` - Create/update documents
- `APPEND` - Add content (comments, logs)
- `ARCHIVE` - Remove from serving (preserves versions)
- `LIST` - Directory contents
- `SEARCH` - Find documents
- `VERSIONS` - Version history

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
- **Specification**: [docs/protocol-spec.md](docs/protocol-spec.md)
- **Design Rationale**: [docs/DESIGN.md](docs/DESIGN.md)

---

*"The web we want, not the web we got."*
