# Demarkus

**A protocol for agents and humans, optimized for information**

Demarkus implements the Mark Protocol: versioned markdown served over QUIC. No rendering pipeline, no tracking, no central authority. Read and write with capability tokens. Every change is traceable. Lightweight and installable anywhere.

Run a single personal server, or compose many into an organizational **knowledge system**: a broker-fronted universe of servers behind one HTTPS endpoint with single sign-on. Humans and agents share the same versioned memory.

## Install

```bash
# macOS / Linux: server + client
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash

# Client only (CLI, TUI, MCP)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

See [full install docs](https://www.demarkus.io/install/) for platform-specific guides and other options.

### See it in action

![Client install demo](docs/images/demarkus-client.gif)

## Quick Start

```bash
# Fetch a document from the live soul server
demarkus mark://soul.demarkus.io/index.md

# Browse interactively with the TUI
demarkus-tui mark://soul.demarkus.io/index.md

# Run your own local server
demarkus-server -root ./docs/site
```

For more examples (tokens, publishing, editing), see [full usage guide](https://www.demarkus.io/).

## What's Included

| Binary | Purpose |
|--------|---------|
| `demarkus-server` | QUIC server with versioned document store, capability-based auth |
| `demarkus-token` | Generate and manage write tokens |
| `demarkus-publish` | Direct-to-store writer for read-only server installs (chroot/RO mode) |
| `demarkus` | CLI tool for all protocol operations (fetch, publish, append, graph, okf, etc.) |
| `demarkus-tui` | Terminal browser: markdown rendering, link navigation, persistent graph |
| `demarkus-mcp` | MCP server for LLM agents (protocol verbs + graph crawling, backlinks, indexing) |
| `demarkus-agent` | Federation agent: crawls worlds, aggregates the link graph, publishes hub indexes |
| `demarkus-broker` | OIDC-fronted MCP gateway that composes many worlds into one knowledge system |

## Protocol at a Glance

**Transport**: QUIC (UDP 6309) | **Scheme**: `mark://` | **Content**: Markdown + YAML frontmatter

Request:

```text
FETCH /hello.md
```

Response:

```text
---
status: ok
version: 3
modified: 2026-01-15T10:30:00Z
---

# Hello World
```

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `LOOKUP` · `PUBLISH` · `APPEND` · `ARCHIVE`

**Open Knowledge Format**: A demarkus document is content-compatible with [Google's Open Knowledge Format (OKF) v0.1](https://github.com/GoogleCloudPlatform/knowledge-catalog) — recognized OKF fields (`type`, `title`, `description`, `resource`, `tags`, `timestamp`) are stored as plain frontmatter, and the server types every document by default. At the system level demarkus is a superset, layering versioning, hash-chain integrity, QUIC transport, and capability auth around an OKF-compatible document. The `demarkus okf` subcommand validates, imports, and exports OKF bundles. See [SPEC §14](docs/SPEC.md).

## Use Cases

**Agent Memory**: Run a server as persistent memory across agent sessions. The Demarkus project itself uses this pattern at `mark://soul.demarkus.io` for architecture notes, debugging lessons, and journal entries. Hit the ground running with the [Claude Code plugin](plugins/claude-code/) or install via [OpenClaw](https://www.demarkus.io/install/openclaw/).

**Organizational Knowledge System**: Compose many servers ("worlds") into a broker-fronted universe reachable through one HTTPS endpoint with OIDC single sign-on. A whole team joins with a single command, `/knowledge-join` from the [Claude Code knowledge plugin](plugins/claude-code-knowledge/), and their agents share organizational memory over MCP, with no per-developer server or token setup. Humans browse the same universe in the [web reading room](https://github.com/latebit-io/demarkus-library). See the [knowledge system scenario](https://www.demarkus.io/scenarios/knowledge-system/).

**Personal Knowledge Base**: Local server, versioned documents, TUI browser. Everything from first write.

**Public Documentation**: Deploy on a VPS, share links, gate writes with tokens.

**Demarkus Hubs**: Link to content on other servers, building a federated directory of knowledge. Hubs can link to hubs.

## Ecosystem

- [demarkus-library](https://github.com/latebit-io/demarkus-library): web reading room; server-rendered front-end for browsing worlds through the broker (SSO) or direct QUIC
- [Caztor](https://github.com/kevinboone/caztor): cross-platform Java GUI browser with preliminary Demarkus support
- [Obsidian plugin](https://github.com/latebit-io/obsidian-demarkus): publish and browse from Obsidian
- [Claude Code memory plugin](plugins/claude-code/): zero-config local memory for Claude Code
- [Claude Code knowledge plugin](plugins/claude-code-knowledge/): join an organizational knowledge system (broker-fronted, MCP OAuth)
- [OpenCode memory plugin](plugins/opencode-memory/): the same local memory for OpenCode (one shared soul across harnesses). Install:

  ```bash
  curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
  ```

- [pi memory plugin](plugins/pi-memory/): the same local memory for the pi coding agent
- [pi knowledge plugin](plugins/pi-knowledge/): join an organizational knowledge system from pi
- [OpenClaw skill](https://clawhub.ai/ontehfritz/demarkus): ClawHub skill for OpenClaw agents (see [install guide](https://www.demarkus.io/install/openclaw/))

See [www.demarkus.io/ecosystem](https://www.demarkus.io/ecosystem/) for the full list.

## Build from Source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all   # or: make server / make client
```

Requires Go 1.26+. Binaries land in `server/bin/`, `client/bin/`, and `tools/bin/`.

## Documentation

- [Website](https://www.demarkus.io/): install guides, scenarios, troubleshooting
- [Protocol Specification](docs/SPEC.md): complete wire format
- [Design Rationale](docs/DESIGN.md): why things are built the way they are

## Core Principles

1. **Optimized for Information**: Markdown is the common language: structured enough for agents, readable enough for humans
2. **Privacy First**: No user tracking, minimal logging, anonymity by default
3. **Security Minded**: Encryption mandatory, capability-based auth, secure by default
4. **Simplicity**: Human-readable protocol, minimal complexity
5. **Anti-Commercialization**: No ads, no tracking, no central authority
6. **Federation**: Anyone can run a server, content can be mirrored freely

## License

- Implementation: AGPL-3.0-only ([LICENSE](LICENSE))
- Protocol Specification: CC0-1.0 ([LICENSE-PROTOCOL](LICENSE-PROTOCOL))

---

*"The web we want, not the web we got."*
