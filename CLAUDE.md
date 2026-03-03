# CLAUDE.md

## demarkus-soul

All project context — architecture, patterns, build commands, conventions, debugging notes, and roadmap — lives on the demarkus-soul MCP server.

### Required Preflight (Every Session)

1. `mark_fetch` `/index.md` — get the hub page
2. `mark_fetch` `/patterns.md` — build commands, code style, workflow
3. Fetch other pages as needed: `/architecture.md`, `/debugging.md`, `/roadmap.md`
4. If MCP is unavailable, stop and ask the user before proceeding

### During Work

- Update soul pages when learning something new
- Use `mark_append` for journal entries and incremental notes
- Always use `expected_version` from a prior fetch when publishing or appending

### End of Session

- Add a journal entry to `/journal.md` if something significant happened

### Content Structure

```
/index.md          — Hub page, links to all sections
/architecture.md   — System design, module boundaries, key decisions
/patterns.md       — Code patterns, build commands, conventions, workflow
/debugging.md      — Lessons from bugs and investigations
/roadmap.md        — What's done, what's next
/journal.md        — Session notes and evolution log
/thoughts.md       — Agent reflections and ideas
/guide.md          — Setup instructions for demarkus-soul
```
