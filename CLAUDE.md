# CLAUDE.md

## demarkus-soul

All project context — architecture, patterns, build commands, conventions, debugging notes, and roadmap — lives on the demarkus-soul MCP server.

### Required Preflight (Every Session)

1. `mark_fetch` `/index.md` — get the hub page
2. `mark_fetch` `/patterns.md` — build commands, code style, workflow
3. `mark_fetch` `/guidelines.md` — hard rules for code quality, must read before writing code
4. Fetch other pages as needed: `/architecture.md`, `/debugging.md`, `/roadmap.md`
5. If MCP is unavailable, stop and ask the user before proceeding

### During Work

- Update soul pages when learning something new
- Use natural language; avoid overusing "-" within sentences. For bullet points, "-" is still preferred.
- Use `mark_append` for journal entries and incremental notes
- Always use `expected_version` from a prior fetch when publishing or appending

### End of Session

- Add a journal entry to `/journal.md` if something significant happened

### Content Structure

```
/index.md          — Hub page, links to all sections
/architecture.md   — System design, module boundaries, key decisions
/patterns.md       — Code patterns, build commands, conventions, workflow
/guidelines.md     — Hard rules for code quality, must read before writing code
/debugging.md      — Lessons from bugs and investigations
/roadmap.md        — What's done, what's next
/journal.md        — Session notes and evolution log
/thoughts.md       — Agent reflections and ideas
/guide.md          — Setup instructions for demarkus-soul
```
### Plans
 - Agents should never store the plans in their own vendor specific folder
 - The agent should always publish the plan to demarkus soul instead
