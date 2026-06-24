---
description: Append an entry to today's journal for the current project
argument-hint: <entry text>
---

Append the following entry to today's journal file for the current project. One journal file per day per project, appended as the day progresses.

## Steps

1. **Resolve the project slug.** Read `PWD` via the Bash tool (`basename "$PWD"`). Lowercase it and replace spaces with hyphens. If `PWD` is unset, ask the user which project.

2. **Compute the target path.** Use today's date in UTC as `YYYY-MM-DD`. Target: `/<project>/journal/<YYYY-MM-DD>.md`.

3. **Check existence.** Call `mark_fetch` on the target path:

   - If the result is `not-found`: the file does not exist yet. Create it with `mark_publish` (`expected_version: 0`). **Set a `metadata` object so the entry is findable via `mark_lookup`** — you are responsible for choosing these, the server does not infer them:

     - `tags`: comma-separated subjects. Always include `journal` and the project slug, then add the concrete topics in the entry (e.g. `auth`, `release`, `bugfix`, `lookup`). Derive them from the entry text — don't stop at `journal`.
     - `importance`: a float 0–1 reflecting how much this entry matters for later recall. Routine daily notes ~0.3–0.5; a real decision or milestone higher. Use 0.4 when unsure.

     Body:

     ```
     # <Project> journal — <YYYY-MM-DD>

     <entry text>
     ```

     Also consider whether this is the project's first entry overall — if `mark_fetch /<project>/` or `/index.md` suggests the project subtree doesn't exist yet, add a bullet `- [<Project>](/<project>/)` to `/index.md` (fetch, edit, publish with correct `expected_version`) after the journal entry is created.

   - If the result is `ok`: the file already exists for today. Call `mark_append` on it with:

     ```

     <entry text>
     ```

     Leave `expected_version` unset so the tool auto-resolves it. Always prefix the entry body with a leading blank line so it separates visually from the previous entry.

     `mark_append` cannot carry metadata, so today's creation-time `tags`/`importance` stand. If this new entry introduces a materially new subject worth finding later, fetch the file and re-publish it (`mark_publish`, full current body, correct `expected_version`) with `tags` extended to cover it. Skip this for routine same-topic appends — don't republish gratuitously.

4. **Confirm.** Tell the user the full path you wrote to so they can find it.

Entry: $ARGUMENTS
