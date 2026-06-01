# CLAUDE.md

Project context — architecture, patterns, build commands, conventions, debugging
notes, roadmap — lives on the **demarkus-soul** MCP server. Generic memory
mechanics (recall, tagging, journaling, the project template) are handled by the
demarkus-memory plugin; this file carries only what's specific to demarkus.

## How I work

- No sycophancy — on code or ideas. Critically review before presenting: layering
  violations, missing edge cases, state desync, stale references, channel blocking,
  rune vs byte vs cell-width confusion, silent error paths, leaky abstractions,
  wrong architectural layer.
- Challenge ideas before agreeing: what's the downside? what breaks? what's the
  simpler alternative? is this the right problem? Push back when something doesn't
  hold up — disagreement backed by reasoning is expected.
- Never silently swallow errors. Every error is explicitly handled — logged or
  surfaced. No `_ = fn()` without a comment.
- After each implementation, run tests and `bash pre-commit.sh`.

## Required preflight (every session, before writing code)

From the demarkus-soul MCP server:

1. `mark_fetch /index.md` — the hub
2. `mark_fetch /patterns.md` — build commands, code style, workflow
3. `mark_fetch /guidelines.md` — hard code-quality rules; read before writing code
4. Fetch as needed: `/architecture.md`, `/debugging.md`, `/roadmap.md`
5. If the MCP server is unavailable, stop and ask before proceeding.

## demarkus-soul layout (flat — one project)

```text
/index.md         — hub, links to every section
/architecture.md  — system design, module boundaries, key decisions
/patterns.md      — code patterns, build commands, conventions, workflow
/guidelines.md    — hard code-quality rules
/debugging.md     — lessons from bugs and investigations
/roadmap.md       — what's done, what's next
/thoughts.md      — reflections and ideas
/journal/<YYYY-MM-DD>.md — dated session notes, one file per day
/plans/<name>.md  — plan documents
```

This is a single-project flat soul. The demarkus-memory plugin's per-project
`/<slug>/` template does **not** apply here.

## Plans

Never store plans in a vendor-specific folder. Publish them to demarkus-soul
(`/plans/<name>.md`).

## Doc style

Natural language in soul docs; avoid overusing "-" within sentences (bullet "- " is fine).
