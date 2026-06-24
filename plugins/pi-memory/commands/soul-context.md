---
description: Restore session context from the current project's soul (recent journal, active work)
---

# /soul-context

Bring the current project's recent memory into the session so the agent and the user can pick up where they left off. Read-only; does not modify the soul.

## Steps

1. **Resolve the project slug.** Read `PWD` via the Bash tool (`basename "$PWD"`). Lowercase it and replace spaces with hyphens. If `PWD` is unset, ask the user which project.

2. **Confirm the project exists in the soul.** Call `mark_fetch /<project>/index.md` (or `mark_list /<project>/`). If the result is `not-found`, tell the user the project has no entries in the soul yet, suggest `/soul-journal "<entry>"` to start one, and stop.

3. **Pull recent journal entries.** List `/<project>/journal/` via `mark_list`. Take today's entry plus the two most recent prior days (UTC `YYYY-MM-DD.md`). For each, `mark_fetch` and read in full. If today's file doesn't exist, just use the most recent two prior days.

4. **Pull active work.** `mark_fetch /<project>/roadmap.md` if it exists (what's next / in flight). Skip silently if `not-found`.

5. **Summarize for the user.** Render in this shape, plain text, no preamble:

   ```text
   ## <Project> — recent context

   ### What's in flight
   <one or two sentences pulled from roadmap.md, or "No roadmap yet">

   ### Recent journal
   - <YYYY-MM-DD>: <one-line summary of that day's entry>
   - <YYYY-MM-DD>: <...>

   ### Pick up where you left off
   <one or two specific suggestions tied to the most recent journal entry — what was the user about to do next?>
   ```

   Keep summaries tight (one line each). The goal is restoring context, not reading entries verbatim.

## Don't

- Don't fabricate. If a section is empty (no roadmap, no recent journals), say so.
- Don't pull every doc in the project — this is a session primer, not a dump.
- Don't write to the soul. This command is read-only.
