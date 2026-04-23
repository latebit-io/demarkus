---
description: Append an entry to the demarkus soul's journal
argument-hint: <entry text>
---

Append the following entry to `/journal.md` in the demarkus-memory soul using the `mark_append` MCP tool. Prefix the entry with a level-2 heading containing today's UTC date in `YYYY-MM-DD` format (skip if the heading already exists today). Leave `expected_version` unset so the tool auto-resolves it. If `/journal.md` does not exist yet, create it with `mark_publish` instead of appending.

Entry: $ARGUMENTS
