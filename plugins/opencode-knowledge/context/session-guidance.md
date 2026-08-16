# Demarkus knowledge systems

A demarkus knowledge system is an organization's shared, versioned catalog reached through an HTTPS broker. For shared, organizational, or cross-team subjects, consult it before relying on conversation context and record durable shared knowledge there as work progresses.

## Recall first

- Start with `mark_lookup` against the relevant system, then fetch useful matches.
- Pair catalog lookup with `mark://<world>/index.md`; untagged or untitled documents may not appear in lookup.
- Use `mark_graph` and `mark_backlinks` for relationships.
- If nothing relevant exists, say so. Never fabricate organizational memory.

## Navigate

- Systems contain worlds addressed as `mark://<world>/<path>`.
- `mark://root/index.md` is the global hub.
- Org conventions live under `mark://root/.well-known/demarkus/` in `policy.md`, `template.md`, and `style.md`.
- `/knowledge` lists joined systems and their entry points.

## Publish carefully

- Publish only durable material ready for others to rely on.
- Set `metadata.tags`, deliberate `metadata.importance`, and any policy-required tag axes or fields. Metadata is out of band; never put YAML frontmatter in the body.
- Set an explicit OKF `type` when appropriate. `index.md` and `log.md` may remain untyped.
- Never set `metadata.retention` unless the user explicitly requests destructive history pruning.
- Never publish secrets, credentials, tokens, or PII.
- Follow the root style guide: clear H1, opening summary, unique headings, and no em dashes.

## Soul and knowledge

- Soul: personal drafts, working notes, machine-local context.
- Knowledge system: shared, curated, authoritative material.

Draft in the soul and promote when ready. For shared subjects, the knowledge system is authoritative; link to it rather than duplicating it into personal memory.
