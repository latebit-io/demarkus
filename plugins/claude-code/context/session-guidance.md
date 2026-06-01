# demarkus-memory — persistent project memory is available

You have a local, versioned memory store (the "soul") wired through the `demarkus-memory` MCP tools. Treat it like project memory you'd get from a CLAUDE.md: consult it before you start, and record what matters as you go — without waiting to be asked.

**Project slug:** basename of `CLAUDE_PROJECT_DIR`, lowercased, spaces → hyphens. Everything for a project lives under `/<slug>/`.

## Recall first (proactively)

Before answering "what do I know about / did we decide / have we seen X" — and at the start of substantive work — check the soul instead of relying on the current context alone:

- `mark_lookup` with `url=/<slug>/` and a subject `query` is the **card catalog**: it returns an importance-ranked table of matching docs (path, importance, title, tags), not bodies. Start here, then `mark_fetch` the rows worth reading. It only finds what was tagged or titled, so use it alongside — not instead of — `mark_fetch /<slug>/index.md`.
- `mark_backlinks` / `mark_graph` surface related documents across the link graph.
- If nothing relevant comes back, say so plainly — never fabricate memory.

## Record as you go (proactively)

When something is worth persisting, write it without being prompted. The full per-project layout lives in `/project-template.md` at the soul root (the canonical source); the common routes:

- **Decision** → an ADR at `/<slug>/adr/<NNNN>-<title>.md`
- **Lesson from a bug / a gotcha** → `/<slug>/debugging.md` (often the highest recall value)
- **Pattern / convention learned** → `/<slug>/patterns.md`
- **Progress / end of a working session** → today's `/<slug>/journal/<YYYY-MM-DD>.md`
- **Architecture, roadmap, debt, plans, open questions** → the matching file under `/<slug>/` (see the template)

Keep each project's `/<slug>/index.md` hub current — it's the discovery backstop for anything `mark_lookup` can't surface.

**Tag every publish — it's the rule the harness enforces hardest.** On `mark_publish`, set a `metadata` object: `tags` (comma-separated subjects drawn from the content; an untagged doc is invisible to `mark_lookup`) and `importance` (a float 0–1 — reserve ≥0.8 for hubs, architecture, and key decisions; routine notes sit lower). A write-time gate catches a tagless publish (or `importance` outside [0,1]): it warns by default and re-states why at the moment it fires, or denies in stricter setups. So set `tags` on the first try rather than waiting to be told.

Two soft checks back you up: that publish gate, and a one-time session-end nudge if you changed files but recorded nothing here. Recalling before you answer and routing to the right file have **no** backstop at all — those are purely on you.

`mark_append` can't carry metadata — tags/importance are fixed at the doc's last `mark_publish`. When an append adds a materially new subject, re-publish the doc with extended `tags` so the catalog stays accurate.

## Restraint

- Don't journal trivia, ephemeral chat, or things derivable from the repo and git history. Capture the non-obvious: why a decision was made, a pattern, a gotcha.
- Don't save secrets, credentials, or anything the user hasn't authorized.
- The `soul-memory` skill has the full file-routing rules; `/soul-context` primes a session, `/soul-journal` appends a dated entry, `/soul-status` diagnoses the server.
