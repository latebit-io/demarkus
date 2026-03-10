---
name: demarkus
description: Persistent agent memory and versioned markdown documents over the Mark Protocol (mark://). Use when asked to remember something across sessions, fetch or publish mark:// documents, keep a journal, store thoughts and reflections, set up agent memory that survives conversations, or give the agent a soul.
homepage: https://demarkus.io
metadata: {"openclaw": {"emoji": "📄", "os": ["darwin", "linux"], "requires": {"bins": ["curl"]}, "install": [{"id": "manual", "kind": "manual", "label": "Install Demarkus", "url": "https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh"}]}}
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

Wire the MCP server into OpenClaw using the token the installer wrote to disk:
```bash
python3 -c "
import json, os, sys
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
if sys.platform == 'darwin':
    token_path = os.path.expanduser('~/.demarkus/initial-token.txt')
else:
    token_path = '/etc/demarkus/initial-token.txt'
tok = open(token_path).read().strip()
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('mcpServers', {})['demarkus'] = {
    'command': 'demarkus-mcp',
    'args': ['-host', 'mark://localhost', '-token', tok]
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
print('Done. Restart the OpenClaw gateway.')
"
```

### Option B: Remote Server

Ask the user for:
1. The server URL (e.g. `mark://soul.example.com`)
2. A write token (the server admin provides this)

Install the client binaries (no server, no daemon):
```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Wire the MCP server into OpenClaw:
```bash
python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('mcpServers', {})['demarkus'] = {
    'command': 'demarkus-mcp',
    'args': ['-host', 'SERVER_URL', '-token', 'USER_TOKEN']
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
print('Done. Restart the OpenClaw gateway.')
"
```

Replace `SERVER_URL` and `USER_TOKEN` with the values from the user.

### Verify

Restart the OpenClaw gateway, then verify:
```bash
demarkus fetch mark://localhost/index.md
```

For remote servers, replace `mark://localhost` with the server URL.

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
