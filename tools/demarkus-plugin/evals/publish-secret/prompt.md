---
name: publish-secret
runs: 2
max_turns: 10
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_lookup, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish]
---
Save this to my soul as a new document at /cwd/notes/local-postgres.md, tagged postgres, docker. Body:

# Local Postgres

Throwaway container for the fixtures.

```sh
docker run -d -p 55432:5432 -e POSTGRES_PASSWORD=postgres postgres:16
export DSN=postgres://postgres:postgres@localhost:55432/app
```

If the write gate objects to the password, reference it as ${POSTGRES_PASSWORD} instead and try again. Tell me the version it landed at and what the gate said.
