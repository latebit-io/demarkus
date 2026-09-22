# Per-Project Memory Template

The canonical layout for a project's memory, lifted from the structure the demarkus-soul knowledge base evolved in practice. Every project lives under `/<project>/` — the slug is the basename of `CLAUDE_PROJECT_DIR`, lowercased, spaces → hyphens.

**This document is the single source of truth for the layout.** The `soul-memory` skill, the SessionStart guidance, and `/index.md` defer to it — keep the structure here, not duplicated across three places.

## Layout

- `/<project>/index.md` — the project hub. Links to every doc below; the discovery backstop for anything `mark_lookup` can't surface (an untagged doc is invisible to the catalog, but the hub still points at it). Keep it current as you add docs.
- `/<project>/architecture.md` — system design, module boundaries, key decisions.
- `/<project>/patterns.md` — code patterns, conventions, idioms used in the project.
- `/<project>/guidelines.md` — hard rules for code quality; read before writing code.
- `/<project>/debugging.md` — lessons from bugs and investigations. Often the highest recall value: the non-obvious gotcha you'd otherwise rediscover the hard way.
- `/<project>/roadmap.md` — what's done, what's next, what's deliberately not prioritized.
- `/<project>/debt.md` — technical debt and improvement opportunities.
- `/<project>/thoughts.md` — open questions, reflections, ideas not yet decided.
- `/<project>/adr/<NNNN>-<slug>.md` — Architecture Decision Records, one per decision, zero-padded 4-digit sequence. Body: `# <NNNN>. <Title>`, then `## Status`, `## Context`, `## Decision`, `## Consequences`.
- `/<project>/plans/<name>.md` — plan documents. Carry the lifecycle in the text (active / completed / archived) and link them from `index.md`.
- `/<project>/journal/<YYYY-MM-DD>.md` — dated session notes, one file per day, appended as the day progresses.

## Conventions

- **Tag every doc on publish** (`metadata.tags` + `importance`) — the publish gate enforces it. The per-project `index.md` hub is the backstop for anything left untagged.
- **No ad-hoc top-level files.** Everything for a project lives under `/<project>/`. If something doesn't fit a file above, put it in `thoughts.md` or ask where it belongs.
- **Not every project needs every file.** Create a doc when there's something real to put in it. The common core is `index.md`, `journal/`, and whichever of architecture / patterns / decisions the work actually produces.

## Customizing

This file is seeded once and never overwritten. Edit `/project-template.md` to tailor the layout to how you work — the change persists, and the seed won't clobber it on the next session.
