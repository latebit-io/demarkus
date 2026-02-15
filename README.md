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

- Go 1.21 or later
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

**Start a server**:
```bash
cd server
./bin/demarkus-server --config config.example.toml
```

**Run the client**:
```bash
cd client
./bin/demarkus mark://localhost:6309/index.md
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
