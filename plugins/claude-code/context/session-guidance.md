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

**You tag and rank — the server infers nothing.** On every `mark_publish`, set a `metadata` object:
- `tags`: comma-separated subjects describing the doc. Derive them from the content. An untagged document is invisible to `mark_lookup`, so this is not optional — it's what makes the write recallable.
- `importance`: a float 0–1 you choose deliberately. Reserve high values (≥0.8) for genuinely central docs (index hubs, architecture, key decisions); routine notes sit lower. It floats matches in lookup ranking; it never surfaces an unmatched doc, so it's a prior, not an override.

This is **enforced** by a write-time gate, not just guidance: a `mark_publish` with no `metadata.tags` (or an `importance` outside [0,1]) trips the publish gate. By default it warns (a system-reminder asking you to re-publish with tags); stricter setups deny the call until you add them. Either way, set `tags` on the first try — don't make the gate ask twice.

`mark_append` can't carry metadata — tags/importance are fixed at the doc's last `mark_publish`. When an append adds a materially new subject, re-publish the doc with extended `tags` so the catalog stays accurate.

## Restraint

- Don't journal trivia, ephemeral chat, or things derivable from the repo and git history. Capture the non-obvious: why a decision was made, a pattern, a gotcha.
- Don't save secrets, credentials, or anything the user hasn't authorized.
- The `soul-memory` skill has the full file-routing rules; `/soul-context` primes a session, `/soul-journal` appends a dated entry, `/soul-status` diagnoses the server.
