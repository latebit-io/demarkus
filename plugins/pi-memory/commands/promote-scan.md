---
description: Sweep the soul for promotion candidates — high-signal, durable, not-yet-promoted docs worth lifting to the knowledge system — and queue them for the gate
---

# /promote-scan

Scan the soul for documents worth promoting to a shared knowledge system, and present a ranked candidate list. This is the on-demand, in-session form of the pipeline's batch sweep: it automates the "mark a candidate" step across many docs at once, so you do not have to remember what is ready. It **never** promotes on its own — each candidate goes through `/promote`, which owns the human gate. (The fully autonomous batch sweep is a separate scheduled agent, not this command.)

`$ARGUMENTS` optionally scopes to one project slug; empty means scan every project.

## Steps

1. **Detection gate.** Run `"$HOME/.demarkus/bin/demarkus-plugin" registry detect-promote` via Bash. `NONE` → there is nowhere to promote to, so a scan is pointless; say so and stop. Otherwise note the destinations (`knowledge <slug>` and/or `target <slug> <path>`) — candidates can be promoted to any of them via `/promote`.

2. **Gather the already-promoted set (to exclude).** `mark_lookup` the soul with `filter=tag=promoted` (optionally scoped to `/<slug>/`). These already live in the catalog — never re-list them as candidates. Their soul docs also carry a `promoted: mark://…@v<N>` marker; `/soul-refresh` keeps them current, not this command.

3. **Find high-signal candidates.** Within scope, surface documents whose *type* makes them likely shared-team knowledge, skipping anything in the already-promoted set:
   - **ADRs** (`/<slug>/adr/*.md`) — decisions, the highest-signal type. List via `mark_lookup`/`mark_list`.
   - **Architecture and key patterns** — `/<slug>/architecture.md`, durable `patterns.md`/`guidelines.md` sections, and any doc the author tagged for sharing or rated high `importance`.
   - Skip journals, thoughts, debt, and day-to-day notes — those are staging-tier by nature and rarely cross.

4. **Lightly triage each (do not run the full cascade).** For each candidate, judge cheaply: is it durable? broadly useful beyond you? plausibly not already covered in the catalog? Drop the ones that obviously fail — be ruthless; most soul content should stay personal. The real triage/distill/dedup happens inside `/promote` per candidate; here you are only producing a worthwhile shortlist.

5. **Present the ranked list.** Plain text, highest-signal first:

   ```text
   ## Promotion candidates — <scope>

   1. /<slug>/adr/0007-foo.md — <one line: why it's a candidate>
   2. /<slug>/architecture.md — <…>
   ...

   Already promoted (excluded): N docs.   Run /promote <path> on any of the above.
   ```

   If nothing qualifies, say so plainly — an empty sweep is a normal, healthy result, not a failure.

## Don't

- Don't promote anything here — this command only surfaces candidates; `/promote` runs the gate per doc.
- Don't re-list already-promoted docs (the `tag=promoted` set from step 2).
- Don't pad the list to look productive. A ruthless shortlist is the point; if most of the soul qualified, the bar would be wrong.
- Don't scan toward a non-existent destination — stop at the detection gate when no knowledge system is joined.
