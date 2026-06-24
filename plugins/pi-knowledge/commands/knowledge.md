---
description: List the joined demarkus knowledge systems and show each one's root hub index — the entry point for navigating the shared catalog.
argument-hint: "[slug | mark://<world>/ | blank = list everything joined]"
---

Orient in the organizational demarkus knowledge system(s) this installation has joined. This is the knowledge-system counterpart to `/soul` (which shows the local soul's index).

## Scope

- **blank** → list every joined system and show each one's `root` hub index.
- **a slug** (the MCP server name from `"$HOME/.demarkus/bin/demarkus-plugin" registry mcp list`, e.g. `acme`) → focus that one system.
- **a `mark://<world>/` URL** → show that specific world's `index.md` hub.

## Steps

1. **Find the joined systems.** Read the registry:

   ```bash
   cat ~/.demarkus/knowledge-systems 2>/dev/null
   ```

   Each line is a joined system's MCP server slug. If the file is missing or empty, tell the user no knowledge system is joined yet and suggest `/knowledge-join <broker-url>`. Stop here.

2. **Show each system's entry points.** For each slug in scope, use that system's MCP tools (`<slug>_mark_fetch`, `<slug>_mark_lookup`, etc.) with `mark://` URLs:

   - Fetch `mark://root/index.md` (the `root` hub) and render it — this is the system's global hub.
   - Fetch `mark://root/.well-known/demarkus/policy.md` if present, and surface the system's write policy (strictness + required tag axes) in one line so the user knows the bar for publishing.
   - If the system exposes more than one world and lists them on the `root` hub, show that list so the user can see where to go next.

   If a fetch returns `not-found`, say so plainly (e.g. the system hasn't published a `root` index or policy yet) rather than inventing content.

3. **Point the way.** Briefly remind the user how to go deeper:
   - `mark_lookup` against the system for a subject (the card catalog).
   - `mark_fetch mark://<world>/index.md` to anchor on a specific world.
   - `/knowledge-join` to add another system; `"$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-list` to see the joined knowledge systems (the gate's source of truth; `registry mcp list` shows *all* MCP servers, souls included). To fully remove one, run both `"$HOME/.demarkus/bin/demarkus-plugin" registry mcp remove <slug>` and `"$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-unregister <slug>` (the latter stops the publish gate enforcing on it).

## Don't

- Don't fabricate hub or policy content — if a document isn't there, report it as absent.
- Don't confuse this with `/soul`. `/soul` shows the personal, local soul; `/knowledge` shows the shared, broker-fronted knowledge system(s). They are different stores with different audiences.
