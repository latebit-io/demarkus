---
description: Promote a soul document up to a shared knowledge system — curate, route, gate, publish, and back-stamp the source
---

# /promote

Promote one soul document from your personal soul (the staging tier) up to a shared destination (the curated, authoritative tier). A destination is either a **brokered knowledge system** (joined via `/knowledge-join`) or a **plain remote demarkus server** registered as a promote target. This is the bridge between the two plugins: the memory side owns intent, source, registry, and back-stamp; the **execution cascade (triage → distill → dedup → tag → route → gate → publish) is the demarkus-knowledge plugin's `knowledge-promote` skill.** This command orchestrates them and is the manual promotion trigger.

`$ARGUMENTS` is the soul path to promote (e.g. `/demarkus/plans/foo.md`). If empty, ask which document.

## Steps

1. **Detection gate.** Run `"$HOME/.demarkus/bin/demarkus-plugin" registry detect-promote` via Bash. It lists every destination, one per line:
   - `knowledge <slug>` → a brokered knowledge system (the `<slug>` MCP server). Destination-selection there uses `mark_worlds` (writable column) + each world's `world.md`.
   - `target <slug> <path> [label]` → a plain remote endpoint and the write `<path>` you declared for it. The destination is that `<slug>`/`<path>` directly — no `mark_worlds`/`world.md` (a plain server has no directory), default autonomy `human-only`.
   - `NONE` → no destination is configured, so promotion is dormant. Offer the two ways to light it up, and act on the user's choice:
     - a brokered system: `/knowledge-join <broker-url>` (demarkus-knowledge plugin);
     - a plain remote server: register it now — confirm its MCP server slug (from the pi-mcp-adapter server list (`/mcp` or `pi config`)) and the write path/prefix, then run `"$HOME/.demarkus/bin/demarkus-plugin" registry promote-target add <slug> <path> [label]` and continue.
   - If more than one destination is listed, ask which to use; otherwise use the single one. Reach any destination through its `<slug>_mark_*` tools.

2. **Read the source.** `mark_fetch` the soul path from the soul server (`demarkus_memory_mark_fetch` or the configured soul server). Note its current version — the back-stamp needs it. If `not-found`, say so and stop.

3. **Already promoted?** If the source body already carries a `promoted: mark://…@v<N>` marker (see step 6), this document was promoted before. Do not silently re-promote: tell the user where it lives, and only continue if they explicitly want to push an **update** (an edit since the last promotion). An update re-enters the cascade as a gated change to the existing knowledge doc, not a new one — this is the upward leg of the coherence edge that `/soul-refresh` points back here for.

4. **Run the execution cascade (knowledge side).** Hand the source document — and the chosen destination from step 1 (a brokered system, or a plain `target` with its declared write path) — to the demarkus-knowledge plugin's **`knowledge-promote`** skill, which:
   triages (durable? broadly useful? not already in the catalog — most soul content correctly fails here and stays personal), distills for a shared audience (strips personal/local framing and any secrets/PII), dedups and conflict-checks against the destination via `mark_lookup`, tags appropriately (honoring the destination's `policy.md` `require_tags`/`category:` if it has one), selects where to write (a brokered system: a **writable** world via `mark_worlds` + `world.md` routing; a plain target: the declared `<path>`), applies the autonomy ceiling (`human-only` for a plain target), runs the human gate, and publishes with provenance back to this soul origin. It returns the published `mark://<dest>/<path>@<version>`.
   - If the cascade triages the document out (not worth promoting) or the human declines at the gate, report that and **stop without back-stamping** — nothing was published.

5. **Capture the result.** From the cascade, record the published mark — `mark://<dest>/<path>@v<N>`, where `<dest>` is the world slug for a brokered system or the endpoint slug for a plain remote target — and its version.

6. **Back-stamp the soul doc (memory side, this command's job — the knowledge plugin never writes the soul).** Apply provenance to the source so it is not re-promoted and does not drift from the now-authoritative copy. Confirm the mode with the user:
   Both modes carry the same machine-readable marker line so `/soul-refresh` can find the doc later and tell which destination version it tracks. Emit it **verbatim** on its own line, where `<dest>`/`<path>` is the published mark from step 5 and `<N>` its version:

   ```text
   promoted: mark://<dest>/<path>@v<N>
   ```

   (`<dest>` is the destination world slug for a brokered system, or the endpoint slug — registered via `"$HOME/.demarkus/bin/demarkus-plugin" registry promote-target add` — for a plain remote target.)

   - **Stub (default, link-not-copy).** Replace the body with a short stub — the H1 title, the marker line, and a one-line summary — so recall resolves the link and fetches fresh from knowledge. There is then no rival copy to go stale. Best for reference docs you will not keep editing locally.
   - **Marker only.** Keep the body, put the marker line directly under the H1. Best for living documents (e.g. an active plan) you keep iterating in the soul. The `@v<N>` stamp lets `/soul-refresh` detect when the knowledge copy has moved on.

   Write the back-stamp with `mark_publish` on the soul at the version from step 2 (republish the chosen body; preserve existing `tags`/`importance` and **add the `promoted` tag** so the doc is discoverable as promoted via `mark_lookup filter=tag=promoted`). Reconciliation is **directional**: knowledge is authoritative, the soul refreshes from it, and any local edits re-enter through the gate (step 3) — never a silent two-way merge.

7. **Report.** Tell the user the destination `mark://`, the back-stamp mode applied, and the soul path. Reference both by full path.

## Don't

- Don't promote without the human gate (step 4) — a shared authoritative store's autonomy is capped by the destination's `world.md` ceiling, never chosen freely here. Default is human-in-the-loop.
- Don't back-stamp if nothing was published (triaged out or gate declined).
- Don't let the knowledge plugin write the soul, or this command write the knowledge system directly — each side stays in its lane. The back-stamp is one-directional (knowledge reports the `mark://`; memory stamps its own doc).
- Don't promote secrets, credentials, or PII. Distillation strips them; it does not merely summarize around them.
- Don't lower the bar. Most soul content should never promote — if it is not durable and broadly useful to others, it stays personal.
