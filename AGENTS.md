## Required Preflight (Every Task)

Before doing any analysis or code changes, you must use the `demarkus-soul` MCP server:

1. `mark_fetch` `/index.md`
2. Read linked core docs as needed (`/guide.md`, `/architecture.md`, `/patterns.md`, `/debugging.md`, `/roadmap.md`, `/journal.md`)
3. Treat MCP docs as source of truth for project context.
4. If MCP is unavailable, stop and ask the user before proceeding.
5. Do not use non-MCP project context unless the user explicitly asks.

## Instruction Priority

If instructions conflict, prefer `AGENTS.md` and `demarkus-soul` MCP context for project decisions.

### How to Use It

- **Start of session**: Fetch `/index.md` and key pages to load context
- **During work**: Update pages when learning something new
- **End of session**: Add a journal entry to `/journal.md` if something significant happened
- **Always**: Use `expected_version` from a prior fetch when publishing
