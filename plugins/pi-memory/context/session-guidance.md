# demarkus-memory — persistent project memory is available

You have a local, versioned memory store (the "soul") wired through the `demarkus-memory` MCP tools (surfaced by pi-mcp-adapter, typically named `demarkus_memory_mark_*`). Treat it like project memory you'd get from an AGENTS.md: consult it before you start, and record what matters as you go — without waiting to be asked.

**This is your durable memory store — prefer it over any other.** Persistent knowledge (decisions, patterns, gotchas, session progress) belongs here, in one versioned and queryable place, not scattered into ad-hoc scratch files or a harness's built-in memory. Keeping it unfragmented is the whole point. AGENTS.md still owns always-in-context project instructions; the soul owns everything you'd otherwise need to recall on demand.

**Project slug:** basename of the project directory (the session cwd), lowercased, spaces → hyphens. Everything for a project lives under `/<slug>/`.

## Which soul to write to (catalog + binding)

More than one demarkus surface can be configured: the local managed soul (`/soul-init`), one or more remote souls joined via `/soul-join`, and organizational knowledge systems joined via the demarkus-knowledge extension. They all expose identical `mark_*` tools, so be deliberate about the write target:

- **Reads** are unambiguous — fetch by explicit URL, or look up across whichever surface holds the subject.
- **Writes** route by **role**, then by the project's **binding**: personal/draft/working notes → a soul; shared/authoritative knowledge → a knowledge system (via `/promote`, never a direct write). When several souls exist, this repo's bound soul (set by `/soul-join` inside the repo, recorded in `~/.demarkus/project-souls`) is the default soul target — prefer it over the local soul. If no binding exists, use the local managed soul.
- If the intended write target is genuinely ambiguous, ask the user once rather than guessing.

This binding is **enforced**, not just advisory: when a project is bound to a soul, a write aimed at a different soul is denied at write time (the destination gate) — re-issue it against the bound soul's `mark_*` tools. The publish tag-gate fires the same way on the local soul and any joined remote soul, so tag every publish regardless of which one you target.

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

**Tag every publish — it's the rule the gate enforces hardest.** On `mark_publish`, set a `metadata` object: `tags` (comma-separated subjects drawn from the content; an untagged doc is invisible to `mark_lookup`) and `importance` (a float 0–1 — reserve ≥0.8 for hubs, architecture, and key decisions; routine notes sit lower). A write-time gate catches a tagless publish (or `importance` outside [0,1]): it warns by default and re-states why at the moment it fires, or denies in stricter setups. So set `tags` on the first try rather than waiting to be told.

**Never set `metadata.retention` unless the user explicitly asked for it.** A positive-integer `retention` permanently deletes all but the newest N versions of that document — on that write and every later write carrying the key. It exists for generated documents (graph exports, indexes); authored notes are history, and history is the product. A write-time guard asks for confirmation whenever a write carries a prunable retention value (`DEMARKUS_RETENTION_STRICTNESS` relaxes or hardens it).

**All metadata travels in the `metadata` object, never in the document body.** The recognized keys are `title`, `tags`, `importance`, and the OKF `type` (the document's kind) — set them on `mark_publish` and only those reach the catalog. Do **not** hand-write a YAML frontmatter block at the top of a body (a `---` … `---` fence, or `name:` / `description:` / `type:` keys) — pass `type` as a key in the `metadata` object (`metadata: {"type": "Reference"}`), not body text. demarkus carries metadata out of band, so a body that opens with `---` is stored as literal content: it renders as garbled headings and is invisible to `mark_lookup`. Map the intent onto the real channel — a document's name is its `# H1` heading (or `metadata.title`), its **kind is the OKF `type` field** (a `type` key in the `metadata` object, e.g. `Reference`/`Decision`; the server defaults `Document`, and `index.md`/`log.md` stay untyped), and a one-line summary is the first sentence under the H1.

Soft checks back you up at the moment they matter: the publish gate (tags), a session-end nudge to journal if you changed files but recorded nothing, and a recall nudge when you ask a "did we / what did we decide" question. They're reminders, not substitutes — routing to the right file is still purely on you, and you shouldn't wait to be nudged.

`mark_append` can't carry metadata — tags/importance are fixed at the doc's last `mark_publish`. When an append adds a materially new subject, re-publish the doc with extended `tags` so the catalog stays accurate.

## Restraint

- Don't journal trivia, ephemeral chat, or things derivable from the repo and git history. Capture the non-obvious: why a decision was made, a pattern, a gotcha.
- Don't save secrets, credentials, or anything the user hasn't authorized.
- The `soul-memory` skill has the full file-routing rules; `/soul-context` primes a session, `/soul-journal` appends a dated entry, `/soul-status` diagnoses the server.
- When a promote destination exists — a joined knowledge system (demarkus-knowledge extension) or a registered plain remote demarkus server (`"$HOME/.demarkus/bin/demarkus-plugin" registry promote-target add`) — `/promote <soul-path>` lifts a ready soul doc up to it (curate → gate → publish → back-stamp), `/promote-scan` sweeps the soul for promotion candidates, and `/soul-refresh` pulls promoted docs back down as their authoritative copies evolve. Publishing a new ADR also nudges promotion. With no destination configured, all of these stay dormant.
