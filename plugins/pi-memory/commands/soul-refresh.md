---
description: Refresh promoted soul documents from the knowledge system — the downward leg of the coherence edge (knowledge is authoritative; the soul refreshes from it)
---

# /soul-refresh

Keep promoted soul documents fresh as their published copies evolve. This is the **downward** counterpart to `/promote`: once a soul doc is promoted, the authoritative copy lives in the knowledge system and may move on, leaving the soul's copy stale. This command detects that and refreshes — always **directionally**: knowledge is the base of truth, the soul refreshes *from* it, and local soul edits re-enter *upward* through `/promote`'s gate, never as a silent two-way merge.

`$ARGUMENTS` optionally narrows to one soul path; empty means scan all promoted docs.

## Steps

1. **Detection gate.** Run `"$HOME/.demarkus/bin/demarkus-plugin" registry detect-promote` via Bash. `NONE` → no promote destination is currently configured, so there is nothing to refresh against (any earlier promotions would need their destination re-added first); say so and stop. Otherwise note the destinations (`knowledge <slug>` and/or `target <slug> <path>`). A promoted soul doc records its own `mark://` target in its marker, so refresh works against whichever destination it was promoted to.

2. **Find promoted docs.** If `$ARGUMENTS` names a path, use just that one. Otherwise `mark_lookup` the soul with `filter=tag=promoted` to list every promoted document (the `/promote` back-stamp adds that tag). For each, `mark_fetch` it and parse its marker line:

   ```text
   promoted: mark://<dest>/<path>@v<N>
   ```

   `<dest>` is the world slug for a brokered system, or the endpoint slug for a plain remote target; `<N>` is the destination version this soul copy was last synced to. A doc with the `promoted` tag but no parseable marker is malformed — report it and skip (do not guess a destination).

3. **Check each against the live copy.** Through the destination's MCP server (`<slug>_mark_fetch` / `<slug>_mark_versions`), read the current version of `mark://<dest>/<path>` — call it `<M>`.
   - `<M> == <N>` → in sync. Skip silently.
   - the destination doc is `not-found` (archived or moved) → surface it; the link is dangling. Do not delete the soul doc — tell the user and let them decide. Skip the auto-refresh.
   - `<M> > <N>` → the authoritative copy moved on. Refresh, per the doc's mode (step 4).

4. **Refresh a stale doc — directionally.** Determine the mode from the doc's shape:
   - **Stub (body is just title + marker + summary, no rival content).** Safe to refresh automatically: `mark_fetch` the live authoritative body for the summary, then `mark_publish` the soul stub with the marker bumped to `@v<M>` (and a refreshed one-line summary). The authoritative copy (knowledge system or remote endpoint) wins; there was no local content to lose.
   - **Marker-only (the soul kept a full body).** The local body may have diverged, so do **not** overwrite it silently. Surface the move (`<dest>/<path>` went `v<N>` → `v<M>`) and offer the user two directional choices:
     - **Pull down (authoritative wins):** replace the soul body with the live destination body and bump the marker to `@v<M>`. Use when the soul copy has no local edits worth keeping.
     - **Re-promote (soul proposes upward):** if the soul body has local refinements, those go *up* as a gated update — run `/promote` on this doc (its step 3 already-promoted path routes the edit through the cascade as an update to the existing knowledge doc). Never fold local edits into the authoritative copy without the gate.
   Always confirm before writing. The marker bump is a `mark_publish` on the soul at its current version.

5. **Report.** Per doc: in-sync / refreshed (stub) / surfaced-for-decision (marker-only) / dangling. Reference paths in full. If nothing was stale, say so plainly.

## Don't

- Don't two-way merge. Reconciliation is directional — knowledge authoritative, soul refreshes from it, soul edits re-enter through `/promote`'s gate.
- Don't overwrite a marker-only body without the user's say-so; that is where local edits live.
- Don't delete or archive a soul doc whose knowledge target is missing — surface it, let the user decide.
- Don't chase related-but-distinct staleness here (a separate soul note that merely overlaps a refined knowledge doc). That needs semantic relatedness and is deferred; this command only reconciles a doc with its own promoted copy.
- Don't run the cascade yourself — re-promotion goes through `/promote`, which owns the gate.
