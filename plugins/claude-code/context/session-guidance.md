# demarkus-memory — persistent project memory is available

You have a local, versioned memory store (the "soul") wired through the `demarkus-memory` MCP tools. Treat it like project memory you'd get from a CLAUDE.md: consult it before you start, and record what matters as you go — without waiting to be asked.

**Project slug:** basename of `CLAUDE_PROJECT_DIR`, lowercased, spaces → hyphens. Everything for a project lives under `/<slug>/`.

## Recall first (proactively)

Before answering "what do I know about / did we decide / have we seen X" — and at the start of substantive work — check the soul instead of relying on the current context alone:

- `mark_lookup` with `url=/<slug>/` and a subject `query` is the **card catalog**: it returns an importance-ranked table of matching docs (path, importance, title, tags), not bodies. Start here, then `mark_fetch` the rows worth reading. It only finds what was tagged or titled, so use it alongside — not instead of — `mark_fetch /<slug>/index.md`.
- `mark_backlinks` / `mark_graph` surface related documents across the link graph.
- If nothing relevant comes back, say so plainly — never fabricate memory.

## Record as you go (proactively)

When something is worth persisting, write it without being prompted:

- **Decision** → an ADR at `/<slug>/adr/<NNNN>-<title>.md`
- **Pattern / convention learned** → `/<slug>/patterns.md`
- **Significant progress or end of a working session** → append to `/<slug>/journal/<YYYY-MM-DD>.md`
- **Architecture / roadmap / task changes** → the matching file under `/<slug>/`

**Always set `metadata` on `mark_publish`:** `tags` (comma-separated subjects) and, sparingly, `importance` (0–1, for genuinely central docs). An untagged document is invisible to `mark_lookup` later — tagging on write is what makes recall work.

## Restraint

- Don't journal trivia, ephemeral chat, or things derivable from the repo and git history. Capture the non-obvious: why a decision was made, a pattern, a gotcha.
- Don't save secrets, credentials, or anything the user hasn't authorized.
- The `soul-memory` skill has the full file-routing rules; `/soul-context` primes a session, `/soul-journal` appends a dated entry, `/soul-status` diagnoses the server.
