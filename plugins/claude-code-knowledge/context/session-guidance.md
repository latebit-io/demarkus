A demarkus **knowledge system** is an organization's shared, versioned knowledge base, reached over the broker through the MCP tools listed above. It is the authoritative catalog for work that more than one person relies on. Treat it as first-class: for any subject that is shared, organizational, or cross-team, consult the knowledge system **before** answering from this conversation, and record durable shared knowledge there as you go.

## Consult the knowledge system first (proactively)

For "what does the org know / what's the standard / did the team decide / where do we document X" questions — and at the start of work that touches shared systems — check the knowledge system before relying on the current context:

- `mark_lookup` against the system (the broker MCP server) with a subject `query` is the **card catalog**: an importance-ranked table of matching docs (path, importance, title, tags), not bodies. Start there, then `mark_fetch` the rows worth reading. It only finds what was tagged or titled, so pair it with the world's `index.md` hub.
- `mark_backlinks` / `mark_graph` surface related documents across the link graph.
- If nothing relevant comes back, say so plainly — never fabricate organizational memory.

## Navigate: hubs and worlds

A knowledge system composes one or more **worlds** (demarkus servers). URLs take the form `mark://<worldName>/<path>`.

- Every system has a guaranteed **`root` hub** that tracks things globally. Org-wide conventions live there under a well-known prefix:
  - `mark://root/.well-known/demarkus/policy.md` — write policy (strictness + required tag axes).
  - `mark://root/.well-known/demarkus/template.md` — the canonical per-world layout. Follow it when you create or extend a world.
- Anchor on a world's `mark://<world>/index.md` hub to find your way around it.
- The `/knowledge` slash command lists the systems you've joined and shows each one's `root` hub index — use it to orient quickly.

## Record to the shared catalog (proactively)

When something is useful to another person and ready to rely on, publish it to the knowledge system — follow the world's `template.md` for where it goes. This is a shared catalog, not a scratch pad:

- **Tag every publish.** On `mark_publish` set a `metadata` object: `tags` (comma-separated subjects drawn from the content — an untagged doc is invisible to `mark_lookup`) and `importance` (a float 0–1; reserve ≥0.8 for hubs, architecture, and key decisions). The system may declare required tag axes (e.g. `category:`) in its policy — satisfy each with an `axis:value` tag. A write-time gate enforces this at the system's chosen severity (warn / block / ask), so set tags on the first try.
- Never publish secrets, credentials, tokens, or PII to a shared system.
- Prefer one well-tagged document over scattered fragments.

## The soul ↔ knowledge-system relationship

If you also have a personal **soul** (the demarkus-memory plugin), the two compose; keep them in their lanes:

- **Soul** = personal, local, your drafts and machine-local working notes. Fast, private, yours.
- **Knowledge system** = shared and authoritative. The bar is higher because others depend on it.

Draft and think in the soul; **promote to the knowledge system when it's ready for others** — re-tagged for the shared taxonomy and following the world's template. For a shared/org subject, the knowledge system is the source of truth and the first place to look; the soul is your scratch space and personal backstop. Don't duplicate authoritative org knowledge into the soul — link to it or fetch it fresh.
