# demarkus-memory — persistent project memory is available

You have a local, versioned memory store (the "soul") wired through the `demarkus-memory` MCP tools. Treat it like project memory you'd get from a CLAUDE.md: consult it before you start, and record what matters as you go — without waiting to be asked.

**This is your durable memory store — prefer it over any other.** Persistent knowledge (decisions, patterns, gotchas, session progress) belongs here, in one versioned and queryable place, not scattered into ad-hoc scratch files or Claude Code's built-in memory. Keeping it unfragmented is the whole point. CLAUDE.md still owns always-in-context project instructions; the soul owns everything you'd otherwise need to recall on demand. If Claude Code's built-in memory tool is enabled and the user would rather demarkus be the single source of truth, you may offer to turn it off — but only with their say-so (the one-time setup offer surfaced at session start covers this); never disable it unasked.

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

**All metadata travels in the `metadata` object, never in the document body.** The recognized keys are `title`, `tags`, and `importance` — set them on `mark_publish` and only those reach the catalog. Do **not** hand-write a YAML frontmatter block at the top of a body (a `---` … `---` fence, or `name:` / `description:` / `type:` keys). demarkus carries metadata out of band, so a body that opens with `---` is stored as literal content: it renders as garbled headings and is invisible to `mark_lookup`. Map the intent onto the real channel — a document's name is its `# H1` heading (or `metadata.title`), its kind is a tag (`type:reference`), and a one-line summary is the first sentence under the H1.

Soft checks back you up at the moment they matter: the publish gate (tags), a session-end nudge to journal if you changed files but recorded nothing, and a recall nudge when you ask a "did we / what did we decide" question. They're reminders, not substitutes — routing to the right file is still purely on you, and you shouldn't wait to be nudged.

`mark_append` can't carry metadata — tags/importance are fixed at the doc's last `mark_publish`. When an append adds a materially new subject, re-publish the doc with extended `tags` so the catalog stays accurate.

## Restraint

- Don't journal trivia, ephemeral chat, or things derivable from the repo and git history. Capture the non-obvious: why a decision was made, a pattern, a gotcha.
- Don't save secrets, credentials, or anything the user hasn't authorized.
- The `soul-memory` skill has the full file-routing rules; `/soul-context` primes a session, `/soul-journal` appends a dated entry, `/soul-status` diagnoses the server.
- When a promote destination exists — a joined knowledge system (demarkus-knowledge plugin) or a registered plain remote demarkus server (`scripts/promote-target.sh add`) — `/promote <soul-path>` lifts a ready soul doc up to it (curate → gate → publish → back-stamp), `/promote-scan` sweeps the soul for promotion candidates, and `/soul-refresh` pulls promoted docs back down as their authoritative copies evolve. Publishing a new ADR also nudges promotion. With no destination configured, all of these stay dormant.
