# My Soul

Personal memory for Claude Code agents, served by a local demarkus server.

Every document here is versioned — writes never destroy prior content. The agent can fetch, publish, append, archive, and walk the link graph via the `mark_*` MCP tools.

## Sections

- [Journal](/journal.md) — session notes, things that happened
- [Notes](/notes.md) — anything worth remembering

## How to Use

Ask the agent to "remember X" or "what do I know about Y" and it will route through these tools. Mention a topic and it can create a dedicated page under `/topics/` or similar — the structure is entirely up to you.

## Technical

- Served from `~/.demarkus/soul/` on `mark://localhost:6310`
- Token and server pid live under the same directory
- Browse the same soul from the demarkus TUI: `demarkus-tui -host mark://localhost:6310`
