---
description: Append an entry to the demarkus soul's journal
argument-hint: <entry text>
---

Process the following entry for `/journal.md` in the demarkus-memory soul:

1. `mark_fetch /journal.md` to determine the current state.
2. If fetch returns `not-found`, create the document with `mark_publish` (expected_version=0) containing:
   - `## YYYY-MM-DD` (today's date in UTC)
   - a blank line
   - the entry text
3. If fetch returns `ok`, append with `mark_append` (leave `expected_version` unset so the tool auto-resolves):
   - include `## YYYY-MM-DD` followed by a blank line **only when that heading is not already present in the fetched body for today**
   - then append the entry text under today's section

Entry: $ARGUMENTS
