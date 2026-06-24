---
description: Set this project's default demarkus write target — list the joined-soul catalog, pick one, and save it as the binding the destination gate enforces.
---

Choose which joined soul **this project writes to by default**. The choice is the
per-project binding in `~/.demarkus/project-souls`; the destination gate enforces
it on every `mark_publish` / `mark_append`. This does NOT join a soul or change
any MCP config — it only re-points an already-joined repo. To add a new soul,
run `/soul-join` first.

Reads and one-off writes to a *different* joined soul are unaffected: call that
soul's `<slug>_mark_*` tools directly. This command only moves the default.

## Steps

1. **List the catalog.** Run:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry soul-default --list --bind "${PWD}"
   ```

   - `EMPTY` → no souls joined. Tell the user to run `/soul-join` (or `/soul-init`
     for the local managed soul) and stop.
   - `CATALOG` header → each following line is
     `<id>\t<tier>\t<host>\t<insecure>\t<current>`. `tier` is `local` (the
     plugin-managed soul, id `demarkus-memory`) or `remote`. `current` is `*` for
     the project's existing default.

2. **Ask which soul to make the default** via a single `AskUserQuestion`, one
   option per catalog row (label = id, description = tier + host). Mark the
   current default in its description. Do not invent souls not in the list.

3. **Save it.** With the chosen `<slug>`:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry soul-default --set <slug> --bind "${PWD}"
   ```

   - On `OK <slug>`, confirm: "This project now writes to soul **<slug>** by
     default." If a server was joined mid-session, note that its MCP tools appear
     only after a restart, but the binding itself is live immediately.
   - On `FAIL: <message>`, show it verbatim (e.g. an unjoined slug → run
     `/soul-join` first).

## Don't

- Don't touch the MCP config — this command only moves the default binding.
- Don't read, echo, or store any token.
- Don't edit `~/.demarkus/project-souls` by hand — the script rewrites only this
  project's row (awk upsert keyed on the exact directory), preserving the others.
- Don't use this to join a soul — that's `/soul-join` (remote) or `/soul-init`
  (local managed).
