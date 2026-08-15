---
description: List joined demarkus knowledge systems and show each root hub index.
argument-hint: "[slug | mark://<world>/ | blank = all joined systems]"
---

Orient in joined organizational knowledge systems. This is the shared, broker-fronted counterpart to `/soul`.

## Scope

- Blank: every joined system and each `root` hub.
- Slug: one registered MCP server.
- `mark://<world>/`: one world's `index.md` hub.

## Steps

1. Read `~/.demarkus/knowledge-systems`. Each nonblank, non-comment line is a joined system slug. If none exist, suggest `/knowledge-join <broker-url>` and stop.
2. For each system in scope, use its `<slug>_mark_*` tools:
   - Fetch `mark://root/index.md`.
   - Fetch `mark://root/.well-known/demarkus/policy.md` and summarize strictness, required tag axes, and required fields in one line.
   - Show worlds linked from the root hub.
3. Report `not-found`, authorization failures, and unavailable tools exactly. Never invent hub or policy content.
4. Point deeper with `mark_lookup` for a subject and `mark_fetch mark://<world>/index.md` for a world.

Use `"$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-list` to list gate registrations. Before removing one, confirm with the user because the MCP endpoint catalog is shared with pi. Then run `opencode mcp logout <slug>`, `registry mcp remove <slug>`, and `registry knowledge-unregister <slug>` in that order before restarting OpenCode. Removing the shared MCP row disconnects pi too.

## Don't

- Do not confuse shared knowledge with the personal soul.
- Do not claim a system is reachable until one of its MCP calls succeeds.
