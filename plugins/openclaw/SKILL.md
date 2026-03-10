---
name: demarkus
description: Persistent agent memory and versioned markdown documents over the Mark Protocol (mark://). Use when asked to remember something across sessions, fetch or publish mark:// documents, keep a journal, store thoughts and reflections, set up agent memory that survives conversations, or give the agent a soul.
homepage: https://demarkus.io
metadata: {"openclaw": {"emoji": "📄", "os": ["darwin", "linux"], "requires": {"bins": ["curl", "bash", "jq"], "config": ["~/.demarkus/initial-token.txt", "/etc/demarkus/initial-token.txt", "~/.openclaw/openclaw.json"]}, "install": [{"id": "manual", "kind": "manual", "label": "Install Demarkus", "url": "https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh"}]}}
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

Store the token from the installer and wire the MCP server into OpenClaw:
```bash
if [ "$(uname)" = "Darwin" ]; then
  TOKEN=$(cat ~/.demarkus/initial-token.txt)
else
  TOKEN=$(sudo cat /etc/demarkus/initial-token.txt)
fi

demarkus token add mark://localhost "$TOKEN"

tmp=$(mktemp)
jq '(.mcp.servers //= {}) | .mcp.servers.demarkus = {"command": "demarkus-mcp", "args": ["-host", "mark://localhost", "-insecure"]}' ~/.openclaw/openclaw.json > "$tmp" && mv "$tmp" ~/.openclaw/openclaw.json

echo "Done. Restart the OpenClaw gateway."
```

### Option B: Remote Server

Ask the user for:
1. The server URL (e.g. `mark://soul.example.com`)
2. A write token (the server admin provides this)

Install the client binaries (no server, no daemon):
```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Store the token and wire the MCP server into OpenClaw:
```bash
demarkus token add SERVER_URL USER_TOKEN

tmp=$(mktemp)
jq --arg host "SERVER_URL" '(.mcp.servers //= {}) | .mcp.servers.demarkus = {"command": "demarkus-mcp", "args": ["-host", $host]}' ~/.openclaw/openclaw.json > "$tmp" && mv "$tmp" ~/.openclaw/openclaw.json

echo "Done. Restart the OpenClaw gateway."
```

Replace `SERVER_URL` and `USER_TOKEN` with the values from the user.

### Verify

Restart the OpenClaw gateway, then verify:
```bash
demarkus -insecure mark://localhost/index.md
```

For remote servers, replace `mark://localhost` with the server URL and drop `-insecure`.

## Tools

- `mark_fetch /path.md` — read a document
- `mark_publish /path.md` — write or update (fetch first, use returned version as expected_version)
- `mark_append /path.md` — append content, no fetch required
- `mark_list /` — list documents and directories
- `mark_versions /path.md` — full version history
- `mark_discover` — fetch the server's agent manifest
- `mark_graph /path.md` — crawl links and build a graph
- `mark_backlinks /path.md` — find what links to a document

## The Soul Pattern

Persistent memory across sessions. Your soul is a collection of markdown documents — a journal, thoughts, architecture notes, debugging lessons — that survive across conversations.

On every new session:
1. `mark_fetch /index.md` — orient yourself
2. Do the work
3. `mark_append /journal.md` — record what happened

### Journaling

Use `mark_append /journal.md` to record session notes, key decisions, and what you learned. Each entry should include a date and a brief summary. This is your running log — append freely, never overwrite.

### Thoughts and Reflections

Use `mark_publish /thoughts.md` to store your own reflections, open questions, and ideas. Unlike the journal (which is append-only), thoughts can be rewritten as your understanding evolves. Always fetch first to get the current version.

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

- **Token handling**: The installer writes a random token to `~/.demarkus/initial-token.txt` (macOS) or `/etc/demarkus/initial-token.txt` (Linux). The setup script stores this token in the demarkus token store via `demarkus token add` so it stays out of `~/.openclaw/openclaw.json` and the long-running MCP process args. The MCP binary resolves tokens from the store at runtime and sends them only to the configured Mark server.
- **Config modification**: Setup modifies `~/.openclaw/openclaw.json` to register the MCP server under `mcp.servers.demarkus`. Only this key is added; existing config is preserved.
- **Network**: The install script downloads binaries from `https://github.com/latebit-io/demarkus`. The server listens on all interfaces (`:6309`) — on Linux the installer opens UDP 6309 via ufw when available. In remote mode, the user provides the server URL explicitly.
- **Data storage**: All documents are stored locally on disk (local mode) or on the user-specified remote server. No data is sent to third parties.
