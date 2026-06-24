---
name: knowledge-promote
description: The execution cascade that curates a staged document and publishes it to a shared knowledge system — triage, distill, dedup, tag, route to a writable world, human-gate, publish with provenance. Invoked by the demarkus-memory plugin's /promote command (soul → knowledge), and reusable for any external inflow distilled into a staging tier.
---

# Knowledge Promote — the curation cascade

This is the knowledge-side half of the promote bridge. It takes a **staged source document** (from a personal soul via `/promote`, or any external inflow distilled into a staging tier) and lands it in a shared knowledge system as curated, authoritative, deduped, well-tagged content — or correctly rejects it. One gate, reused by every inflow.

The bar here is high: a shared catalog is not a soul mirror. **Most staged content should never cross** — if much more than ~10–20% promotes, the bar is too low and the catalog loses its curation value. Ruthlessness is the feature.

## Inputs

- The source document body.
- Its origin (the soul `mark://`/path and version, or the external source) — for provenance.
- The destination, resolved by the caller (`/promote`'s detection step). Its **kind** is given by the input shape, and decides the step-5 path:
  - **Brokered knowledge system** — a single MCP server slug (e.g. `knowledge`). Reach it via `<slug>_mark_*`; it provides `mark_worlds` for world discovery.
  - **Plain remote endpoint** — a slug plus a declared write `<path>` (and optional label), as registered via `promote-target.sh add <slug> <path> [label]`. No `mark_worlds`/`world.md`; write under the declared `<path>`.
  Reach either through that server's `<slug>_mark_*` tools.

## The cascade

Run the stages in order. Each is a gate; a document that fails any early stage stops there and is **not** published.

1. **Triage — should this exist in the shared catalog at all?** Three questions: is it *durable* (not ephemeral or session-specific)? is it *broadly useful* to other people, not just you? is it *not already* in the catalog (a quick `mark_lookup` on the destination for its subject)? If any answer is no, stop and report "stays personal" with the reason. This is where most content correctly dies.

2. **Distill — rewrite for a shared audience.** Strip personal framing ("I decided", "my soul"), local/machine-specific detail, and anything only the origin author would understand. Preserve the substance faithfully — distillation is not lossy summary of the decision, it is removal of the personal wrapper. **Strip every secret, credential, token, and PII** — remove them, do not merely summarize around them. If the value cannot survive that stripping, it does not belong in a shared store.

3. **Dedup and conflict-check against the catalog.** `mark_lookup` the destination for the document's subject/tags, then `mark_fetch` any close matches. If an existing doc already covers this, prefer **updating** it (a gated change to that doc) over creating a near-duplicate. If the new content *conflicts* with an existing doc, surface the conflict to the human at the gate rather than silently overwriting — knowledge is the base of truth and reconciliation is directional.

4. **Tag to the destination taxonomy.** Fetch the destination's policy if it has one (cheap, cache for the session): for a brokered system `mark_fetch mark://root/.well-known/demarkus/policy.md`; for a plain remote endpoint `mark_fetch mark://<slug>/.well-known/demarkus/policy.md` (endpoint-level — a plain server has no per-world root). Honor `require_tags:` — every named axis must be satisfied by an `axis:value` tag (at minimum the `category:` axis). A plain remote endpoint may have no policy; then apply sensible subject tags and a `category:` anyway, so the doc stays findable. Add a deliberate `importance` (0–1; reserve ≥0.8 for hubs/architecture/key decisions). Soul tags are loose and personal; re-tag, don't copy them across.

5. **Select where to write.** The destination kind comes from the caller (`/promote`'s detection step):
   - **Brokered knowledge system:** `mark_worlds` → use the **`writable` column** (readable is not writable); only writable worlds are candidates. For each, `mark_fetch mark://<world>/.well-known/demarkus/world.md` and route by matching the document's subject/team against the declared `domain`/`team` — auto-route on a clear match, else a **write-filtered, labeled pick-list**. A world with no descriptor is writable-but-unlabeled (pick-list only). Note its `autonomy_ceiling`.
   - **Plain remote endpoint (`target <slug> <path>`):** there is no broker directory and no `mark_worlds`/`world.md` — the write `<path>` was DECLARED at registration (client-side; the plain-server counterpart to the writable surface). Write under that path; choose a sensible leaf for this document. Default the autonomy ceiling to `human-only`. If the token cannot actually write there, `mark_publish` returns `not-permitted` — surface that to the human rather than guessing another path.

6. **Human gate, capped by the destination.** Effective autonomy is `min(local preference, autonomy_ceiling)`; default is human-in-the-loop. Present the distilled draft, the chosen destination, the tags, and any dedup/conflict findings, and get explicit approval before publishing. Only relax to a model verification step when the destination's ceiling is `verify-then-auto` (and the local preference allows) — then a refute-check (does the distillation hold against its source?) may stand in for the human. `auto` (no gate) is only ever valid for a low-stakes `scratch` world; never an authoritative one. If the human declines, stop and report — nothing is published.

7. **Publish with provenance.** `mark_publish` to the destination mark:
   - **Brokered:** `mark://<chosen-world>/<path>` (the writable world chosen in step 5).
   - **Plain remote:** `mark://<slug>/<declared-path>/<leaf>`, where `<declared-path>` is the registered write path and `<leaf>` is the filename chosen in step 5.
   For an update, fetch first and use the correct `expected_version`; for a new doc use 0. Set `metadata.tags` and `metadata.importance` from step 4 — metadata travels in the `metadata` object, never as a `---` block in the body. Include a provenance line in the body linking back to the origin (the soul `mark://`/path, and the external source if any).

8. **Return the result** to the caller: the published `mark://<dest>/<path>@v<N>` (`<dest>` = the chosen world for a brokered system, or `<slug>/<declared-path>/<leaf>` for a plain remote) and its new version. The caller (memory side) applies the back-stamp to the origin; **this skill never writes the soul.**

## Model routing (for batch/automated use)

Interactively, one agent runs every stage. When this cascade is driven by a batch sweep over many documents, route by cost: cheap model (Haiku) for triage, field extraction, and tagging (the firehose, closed-set classification); strong model (Sonnet/Opus) only for the survivors that reach distillation and dedup (faithfulness + judgment); human for the gate. Never run the strong model over every candidate — triage discards most cheaply. A strong human gate is what makes a cheaper distillation safe.

## Don't

- Don't lower the triage bar to be "helpful" — rejecting is the common, correct outcome.
- Don't publish secrets, credentials, or PII; don't publish to a world you cannot write; don't exceed the destination's `autonomy_ceiling`.
- Don't write the origin soul — return the `mark://` and let the memory side stamp it. The back-stamp is one-directional.
- Don't silently overwrite a conflicting catalog doc — surface the conflict at the gate. Reconciliation is directional: knowledge authoritative, staged content re-enters as a gated update.
