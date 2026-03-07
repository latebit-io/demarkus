---
layout: default
title: Agent Memory (Soul Pattern)
permalink: /scenarios/agent-memory/
---

# Agent Memory (Soul Pattern)

Give Claude Code (or any MCP-compatible LLM agent) persistent memory across sessions — architecture notes, debugging lessons, roadmap, journal.

This is the pattern used by the Demarkus project itself. You can browse the live example at `mark://soul.demarkus.io`.

## What you'll have

- A Demarkus server holding structured markdown docs
- MCP tools (`mark_fetch`, `mark_publish`, `mark_list`, `mark_append`) available to the agent
- Version history of every memory update
- The agent reads context at session start and writes updates at the end

## Setup

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

On macOS, this installs the server and registers it as a launchd service.
On Linux, this installs and enables the systemd service.

### 2. Create the soul directory

```bash
mkdir -p ~/soul
```

Create an initial index:

```bash
cat > ~/soul/index.md << 'EOF'
# Project Soul

- [Architecture](architecture.md) — system design, key decisions
- [Patterns](patterns.md) — conventions and build commands
- [Debugging](debugging.md) — lessons from bugs
- [Roadmap](roadmap.md) — what's done, what's next
- [Journal](journal.md) — session notes
EOF
```

### 3. Generate a publish token

```bash
demarkus-token generate -paths "/*" -ops publish,archive -tokens ~/soul/tokens.toml
```

Copy the raw token from the output — you'll need it for the MCP config.

### 4. Start the soul server on port 6310

Run it alongside your main server (which uses 6309):

```bash
demarkus-server -root ~/soul -tokens ~/soul/tokens.toml -addr :6310
```

Or configure the full installer to use a different port (manual setup recommended for dual-server).

### 5. Configure MCP for your project

Create or update `.mcp.json` in your project root:

```json
{
  "mcpServers": {
    "demarkus-soul": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://localhost:6310",
        "-token", "<your-publish-token>",
        "-insecure"
      ]
    }
  }
}
```

Replace `/path/to/demarkus-mcp` with the actual path (`which demarkus-mcp` to find it).

### 6. Add CLAUDE.md instructions

Tell the agent how to use the soul. Create `CLAUDE.md` in your project:

```markdown
# CLAUDE.md

## Soul

All project context lives on the soul server.

### Preflight (every session)

1. `mark_fetch` `/index.md` — hub page
2. `mark_fetch` `/patterns.md` — build commands and conventions
3. Fetch other pages as needed

### During work

- Use `mark_append` for incremental notes
- Use `mark_publish` when rewriting a section
- Always use `expected_version` from a prior fetch

### End of session

- Add a journal entry to `/journal.md`
```

### 7. Verify

Open a Claude Code session and run:

```
Please fetch mark://localhost:6310/index.md and summarize what you find.
```

The agent should use `mark_fetch` and return the contents of your index.

## Recommended soul structure

```
/index.md          — hub, links to all sections
/architecture.md   — system design and key decisions
/patterns.md       — build commands, code style, conventions
/debugging.md      — lessons from bugs
/roadmap.md        — what's done, what's next
/journal.md        — session notes (append-only)
/thoughts.md       — agent reflections
```

## Using a remote soul server

If you run the soul on a remote host with TLS, remove `-insecure` from the MCP args and use the `mark://` URL with your domain:

```json
{
  "mcpServers": {
    "demarkus-soul": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://soul.yourdomain.com",
        "-token", "<your-token>"
      ]
    }
  }
}
```

## Live example

Browse the Demarkus project's own soul:

```bash
demarkus-tui mark://soul.demarkus.io/index.md
```

## Related

- [Getting Started](/getting-started/)
- [Soul page](/soul/)
- [Agent Install](/agent-install/)
