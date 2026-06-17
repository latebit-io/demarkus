---
description: Promote a soul document up to a shared knowledge system — curate, route, gate, publish, and back-stamp the source
---

Promote one soul document from your personal soul (the staging tier) up to a shared knowledge system (the curated, authoritative tier). This is the bridge between the two plugins: the memory side owns intent, source, and back-stamp; the **execution cascade (triage → distill → dedup → tag → route → gate → publish) is the demarkus-knowledge plugin's `knowledge-promote` skill.** This command orchestrates them and is the manual promotion trigger.

`$ARGUMENTS` is the soul path to promote (e.g. `/demarkus/plans/foo.md`). If empty, ask which document.

## Steps

1. **Detection gate.** Run `${CLAUDE_PLUGIN_ROOT}/scripts/detect-knowledge.sh` via Bash.
   - `NO_KNOWLEDGE` → there is no knowledge system joined, so there is nowhere to promote to. Tell the user promotion is dormant and that `/knowledge-join <broker-url>` (the demarkus-knowledge plugin) lights it up. Stop.
   - `KNOWLEDGE` followed by slugs → those are the joined endpoint slugs (MCP server names). If more than one, ask which destination system; otherwise use the single one. The agent reaches it through that server's `mcp__<slug>__mark_*` tools.

2. **Read the source.** `mark_fetch` the soul path from the soul server (`mcp__demarkus-memory__mark_fetch` or the configured soul server). Note its current version — the back-stamp needs it. If `not-found`, say so and stop.

3. **Already promoted?** If the source body already carries a `promoted → mark://…` marker (see step 6), this document was promoted before. Do not silently re-promote: tell the user where it lives, and only continue if they explicitly want to push an **update** (an edit since the last promotion). An update re-enters the cascade as a gated change to the existing knowledge doc, not a new one.

4. **Run the execution cascade (knowledge side).** Hand the source document to the demarkus-knowledge plugin's **`knowledge-promote`** skill, which:
   triages (durable? broadly useful? not already in the catalog — most soul content correctly fails here and stays personal), distills for a shared audience (strips personal/local framing and any secrets/PII), dedups and conflict-checks against the catalog via `mark_lookup`, re-tags to the system's taxonomy (the `category:` axis and any `require_tags`), selects a destination world (filtered to your **writable** worlds via `mark_worlds`, routed by each world's `world.md` `domain`), applies the destination's autonomy ceiling, runs the human gate, and publishes with provenance back to this soul origin. It returns the published `mark://<world>/<path>@<version>`.
   - If the cascade triages the document out (not worth promoting) or the human declines at the gate, report that and **stop without back-stamping** — nothing was published.

5. **Capture the result.** From the cascade, record the published `mark://<world>/<path>` and its version.

6. **Back-stamp the soul doc (memory side, this command's job — the knowledge plugin never writes the soul).** Apply provenance to the source so it is not re-promoted and does not drift from the now-authoritative copy. Confirm the mode with the user:
   - **Stub (default, link-not-copy).** Replace the body with a short stub — the H1 title plus `promoted → mark://<world>/<path>@<version>` and a one-line summary — so recall resolves the link and fetches fresh from knowledge. There is then no rival copy to go stale. Best for reference docs you will not keep editing locally.
   - **Marker only.** Keep the body, prepend a `promoted → mark://<world>/<path>@<version>` line under the H1. Best for living documents (e.g. an active plan) you keep iterating in the soul. The `@<version>` stamp lets a later check detect when the knowledge copy has moved on.
   Write the back-stamp with `mark_publish` on the soul at the version from step 2 (republish the chosen body; metadata `tags`/`importance` preserved). Reconciliation is **directional**: knowledge is authoritative, the soul refreshes from it, and any local edits re-enter through the gate (step 3) — never a silent two-way merge.

7. **Report.** Tell the user the destination `mark://`, the back-stamp mode applied, and the soul path. Reference both by full path.

## Don't

- Don't promote without the human gate (step 4) — a shared authoritative store's autonomy is capped by the destination's `world.md` ceiling, never chosen freely here. Default is human-in-the-loop.
- Don't back-stamp if nothing was published (triaged out or gate declined).
- Don't let the knowledge plugin write the soul, or this command write the knowledge system directly — each side stays in its lane. The back-stamp is one-directional (knowledge reports the `mark://`; memory stamps its own doc).
- Don't promote secrets, credentials, or PII. Distillation strips them; it does not merely summarize around them.
- Don't lower the bar. Most soul content should never promote — if it is not durable and broadly useful to others, it stays personal.
