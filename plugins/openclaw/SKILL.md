---
name: demarkus
description: Persistent agent memory and versioned markdown documents over the Mark Protocol (mark://). Use when asked to remember something across sessions, fetch or publish mark:// documents, keep a journal, store thoughts and reflections, set up agent memory that survives conversations, or give the agent a soul.
homepage: https://demarkus.io
license: MIT-0
metadata: {"openclaw": {"emoji": "📄", "os": ["darwin", "linux"], "requires": {"anyBins": ["mcporter", "npx"], "bins": ["curl", "bash"], "config": ["~/.demarkus/initial-token.txt", "/etc/demarkus/initial-token.txt"]}, "install": [{"id": "manual", "kind": "manual", "label": "Install Demarkus", "url": "https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh"}, {"id": "node", "kind": "node", "package": "mcporter", "bins": ["mcporter"], "label": "Install mcporter (node)"}]}}
---

## Setup

First, ask the user: **local server or remote server?**

- **Local** — install and run demarkus on this machine (default)
- **Remote** — connect to an existing demarkus server (the user must provide the server URL and a write token)

### Option A: Local Server

Check if already installed:
```bash
which demarkus
```

If not found, install the full stack (client, server, MCP binary, daemon):
```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

Store the token and register demarkus-mcp with mcporter:
```bash
if [ "$(uname)" = "Darwin" ]; then
  TOKEN=$(cat ~/.demarkus/initial-token.txt)
else
  TOKEN=$(sudo cat /etc/demarkus/initial-token.txt)
fi

demarkus token add mark://localhost "$TOKEN"

mcporter config add demarkus \
  --command demarkus-mcp \
  --arg -host --arg "mark://localhost" \
  --arg -insecure \
  --scope home
```

### Option B: Remote Server

Ask the user for:
1. The server URL (e.g. `mark://soul.example.com`)
2. A write token (the server admin provides this)

Install the client binaries (no server, no daemon):
```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Store the token and register demarkus-mcp with mcporter:
```bash
demarkus token add SERVER_URL USER_TOKEN

mcporter config add demarkus \
  --command demarkus-mcp \
  --arg -host --arg "SERVER_URL" \
  --scope home
```

Replace `SERVER_URL` and `USER_TOKEN` with the values from the user.

### Verify

```bash
mcporter list demarkus --schema
```

For a quick test:
```bash
mcporter call 'demarkus.mark_fetch(url: "/index.md")'
```

## About `mcporter`

- When command `mcporter` does not exist, use `npx -y mcporter` instead.
- https://github.com/steipete/mcporter/raw/refs/heads/main/docs/call-syntax.md
- https://github.com/steipete/mcporter/raw/refs/heads/main/docs/cli-reference.md

## Tools

- `demarkus.mark_fetch` — read a document
- `demarkus.mark_publish` — write or update (fetch first, use returned version as expected_version)
- `demarkus.mark_append` — append content, no fetch required
- `demarkus.mark_list` — list documents and directories
- `demarkus.mark_versions` — full version history
- `demarkus.mark_discover` — fetch the server's agent manifest
- `demarkus.mark_graph` — crawl links and build a graph
- `demarkus.mark_backlinks` — find what links to a document

## Usage

```shell
# Fetch a document
mcporter call 'demarkus.mark_fetch(url: "/index.md")'

# List documents
mcporter call 'demarkus.mark_list(url: "/")'

# Append to journal
mcporter call 'demarkus.mark_append(url: "/journal.md", body: "## 2026-03-10\nSession notes here.")'

# Publish a document (fetch first to get version)
mcporter call 'demarkus.mark_publish(url: "/thoughts.md", body: "# Thoughts\nNew content.", expected_version: 3)'
```

## The Soul Pattern

Persistent memory across sessions. Your soul is a collection of markdown documents — a journal, thoughts, architecture notes, debugging lessons — that survive across conversations.

On every new session:
1. `mcporter call 'demarkus.mark_fetch(url: "/index.md")'` — orient yourself
2. Do the work
3. `mcporter call 'demarkus.mark_append(url: "/journal.md", body: "...")'` — record what happened

### Journaling

Use `mark_append` to record session notes, key decisions, and what you learned. Each entry should include a date and a brief summary. This is your running log — append freely, never overwrite.

### Thoughts and Reflections

Use `mark_publish` to store your own reflections, open questions, and ideas. Unlike the journal (which is append-only), thoughts can be rewritten as your understanding evolves. Always fetch first to get the current version.

### Recommended Structure

```
/index.md          — hub page linking to all sections
/journal.md        — session notes and evolution log (append-only)
/thoughts.md       — your reflections, ideas, open questions
/architecture.md   — system design and key decisions
/patterns.md       — code patterns, conventions, workflow
/debugging.md      — lessons from bugs and investigations
/roadmap.md        — what's done, what's next
```

Use `mark_append` for journals and running notes — cheaper than fetch + republish.
Never publish without fetching first — the server enforces optimistic concurrency.

## Security and Privacy

- **Token handling**: The installer writes a random token to `~/.demarkus/initial-token.txt` (macOS) or `/etc/demarkus/initial-token.txt` (Linux). The setup script stores this token in the demarkus token store via `demarkus token add` so it stays out of config files and long-running process args. The MCP binary resolves tokens from the store at runtime and sends them only to the configured Mark server.
- **MCP bridge**: mcporter spawns `demarkus-mcp` via stdio. No tokens appear in mcporter's config — they are resolved from the demarkus token store at runtime.
- **Network**: The install script downloads binaries from `https://github.com/latebit-io/demarkus`. The server listens on all interfaces (`:6309`) — on Linux the installer opens UDP 6309 via ufw when available. In remote mode, the user provides the server URL explicitly.
- **Data storage**: All documents are stored locally on disk (local mode) or on the user-specified remote server. No data is sent to third parties.
