# World Descriptor — <worldName>

descriptor_version: 1
team: <owning team / contact, e.g. platform-eng (#platform)>
domain: <subject domain(s), comma-separated, e.g. infrastructure, deployment>
partition_role: team
autonomy_ceiling: human-only

How a world describes itself. Unlike `policy.md` and `template.md`, which live once on `root` and govern the whole system, this descriptor is **per-world**: each world publishes its own copy to `mark://<thisWorld>/.well-known/demarkus/world.md`. The world owner is the source of truth — a world declares its own scope, self-sovereign. `root` may aggregate these into a discovery index, but only when **derived** (crawled from the per-world descriptors), never hand-maintained, or the central map drifts from what worlds actually hold.

The five lines above are the **structured core** the curation pipeline reads when routing a promotion. Everything below is convention.

## The core fields

- `descriptor_version` — schema version of this file. `1` today.
- `team` — the owning team or contact. Maps to the human who approves writes here (the `owner:` in the system `policy.md`). Decide who approves before wiring promotion triggers at this world.
- `domain` — the subject area(s) this world holds, comma-separated. Destination-selection matches a promotion candidate's subject against this to auto-route, or to label the world in a pick-list when routing is ambiguous.
- `partition_role` — the world's place in the topology: `hub` (the global tracker; usually `root`), `team` (a team's shared catalog), `project` (a single project's docs), or `scratch` (low-stakes, experimental). Keep the set small and stable.
- `autonomy_ceiling` — the most autonomous a promotion *into this world* may be, set by the world owner. The promotion pipeline applies `min(local preference, autonomy_ceiling)`, so a client can never exceed what the destination allows:
  - `human-only` — every promotion is approved by a human before publish. The safe default; `root` and any authoritative world should stay here.
  - `verify-then-auto` — a model verification step (a refute-check against the source) may stand in for the human.
  - `auto` — publish without a gate. Only for a low-stakes `scratch` world; never an authoritative one.

## How the pipeline consumes it

When promoting a soul document up to this knowledge system, the destination-selection step:

1. Enumerates the worlds the caller may **write** (the `writable` column of `mark_worlds` — readable is not writable).
2. Fetches each writable world's `world.md`.
3. Routes the candidate by matching its subject/team against each world's declared `domain`/`team` — auto-routing on a clear match, or presenting a write-filtered, labeled pick-list otherwise.
4. Caps promotion autonomy at the chosen world's `autonomy_ceiling`.

A world with no descriptor still works: it is writable-but-unlabeled, so it only ever appears in the manual pick-list and defaults to `human-only`. The descriptor is what lets routing be automatic and lets an owner relax the gate deliberately.

## Why per-world, not on root

Partitioning is **discovered, not designed**: where knowledge lands falls out of who can write where (token-encoded access) plus what each world says it holds. The pipeline conforms to that topology; it does not invent a partition scheme. Hosting the descriptor on the world it describes keeps the declaration next to the authority that owns it, and lets the root index be a derived view rather than a second source of truth that can drift.
