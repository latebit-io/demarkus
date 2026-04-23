# My Soul

Personal memory for Claude Code agents, served by a local demarkus server.

Every document here is versioned — writes never destroy prior content. The agent can fetch, publish, append, archive, and walk the link graph via the `mark_*` MCP tools.

## Sections

- [Journal](/journal.md) — session notes, things that happened
- [Notes](/notes.md) — anything worth remembering

## How to Use

Ask the agent to "remember X" or "what do I know about Y" and it will route through these tools. Mention a topic and it can create a dedicated page under `/topics/` or similar — the structure is entirely up to you.

## Technical

- The soul directory and port depend on the setup mode (shared by default, isolated when 6310 is taken, or reuse of an existing server). See `~/.demarkus/plugin-memory.conf` for `SOUL_DIR`, `PORT`, and `MODE`.
- The plugin's auth token is at `~/.demarkus/plugin-memory.token` (mode 0600).
- Browse the same soul from the demarkus TUI using the port from the config: `demarkus-tui -host mark://localhost:$PORT`.
- Run `/soul-init` in Claude Code to reconfigure (switch modes, adopt an existing server, etc.).
