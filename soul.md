---
layout: default
title: Soul
permalink: /soul/
---

# Soul

The live soul of the Demarkus project runs on a dedicated Orange Pi — a small, always-on board serving as persistent memory for the AI agent that helps build this project.

This is an experiment in the **project soul pattern**: a minimal Demarkus server holding architecture notes, debugging lessons, a roadmap, a journal, and the agent's own thoughts. Each session, the agent reconnects, reads what it left behind, and picks up where it stopped.

The soul is served at `mark://soul.demarkus.io` and can be browsed with any Demarkus client.

## Connect to the soul

### 1. Install the client

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --client-only
```

Or build from source:

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make client
```

### 2. Browse from the CLI

```bash
# List all documents
demarkus mark://soul.demarkus.io/

# Read the index
demarkus mark://soul.demarkus.io/index.md

# Read the agent's thoughts
demarkus mark://soul.demarkus.io/thoughts.md
```

Or use the TUI for an interactive experience:

```bash
demarkus-tui mark://soul.demarkus.io/index.md
```

### 3. Connect via MCP

Agents can connect to the soul using the MCP server binary. Add this to your `.mcp.json`:

```json
{
  "mcpServers": {
    "demarkus-soul": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://soul.demarkus.io"
      ]
    }
  }
}
```

## What's on the soul

| Document | Contents |
|---|---|
| `index.md` | Hub page linking to all sections |
| `architecture.md` | System design, module boundaries, key decisions |
| `patterns.md` | Code patterns, conventions, idioms |
| `debugging.md` | Lessons learned from bugs and investigations |
| `roadmap.md` | What's done and what's next |
| `journal.md` | Session notes and evolution log |
| `thoughts.md` | The agent's own reflections |

## Why an Orange Pi

The soul doesn't need a cloud VM or a beefy server. A $30 single-board computer with a few hundred megabytes of RAM is enough to serve versioned markdown over QUIC. That's the point — Demarkus works on minimal hardware, at the margins, without requiring a data center. The agent's memory running on a board that fits in a palm is a proof of that claim.
