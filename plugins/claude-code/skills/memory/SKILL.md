---
name: memory
description: Use when the user wants to remember, save, recall, or query personal notes that should persist across Claude Code sessions. Routes through the demarkus-memory MCP tools against a local versioned markdown store.
---

# Memory

This skill routes "remember" / "recall" intents through the `demarkus-memory` MCP server (a local demarkus-server), which provides a versioned, link-graph-aware personal memory store.

## When to trigger

Trigger on phrases and intents like:
- "remember this", "save to memory", "note that", "jot down"
- "what do I know about X", "do I have notes on Y", "recall Z"
- "add this to my notes", "put this in my soul"
- "find anything about ...", "what have I saved about ..."

Do NOT trigger for ephemeral context ("remember that in this conversation we're using Python 3.12") — that belongs in the current session, not the persistent store.

## Tool routing

Read intents → start with `mark_fetch` on the most likely path. If the user is exploring ("what do I know about X"):
1. Fetch `/index.md` first to see what structure exists
2. If there is a topic page matching the query (e.g. `/topics/x.md`), fetch it
3. Consider `mark_backlinks` or `mark_graph` to surface related documents
4. If nothing relevant is found, say so honestly rather than fabricating

Write intents — decide between append and publish:
- **Append** (`mark_append /notes.md`, `expected_version` left unset to auto-resolve): quick notes, observations, one-liners, fleeting thoughts. If `/notes.md` does not exist yet, create it first with `mark_publish` and `expected_version: 0`, then append on subsequent calls.
- **Publish** (`mark_publish`): when the content is structured enough to deserve its own page — a topic, a person, an ongoing project. Suggest a path like `/topics/<slug>.md` or `/projects/<slug>.md` and ask the user if unsure. Use `expected_version: 0` for new documents; fetch first to get the version when updating existing pages.
- **Journal** (`mark_append /journal.md`): dated session notes, "today I learned" entries. Prefix with a `## YYYY-MM-DD` heading when starting a new day. If `/journal.md` is missing, create it with `mark_publish` (`expected_version: 0`) before appending — or just use the `/soul-journal` command which handles this.

Always reference what you saved by path so the user can find it again.

## Structure conventions

The soul starts with a minimal `/index.md`. As it grows, encourage lightweight structure:

- `/index.md` — hub page with links to everything else
- `/notes.md` — append-only catch-all for fleeting notes
- `/journal.md` — dated entries
- `/topics/<slug>.md` — one page per topic
- `/projects/<slug>.md` — one page per project
- `/people/<slug>.md` — one page per person

When you create a new topic/project/person page, add a link to it from `/index.md` (fetch the index, add the link, publish with the correct `expected_version`).

## Don't

- Don't fabricate memory. If `mark_fetch` returns 404, the document does not exist — say so.
- Don't invent expected_versions. Use 0 for new documents; fetch first when updating.
- Don't bypass the MCP tools with shell commands — the soul is the source of truth.
- Don't save secrets, credentials, or anything the user has not explicitly authorized.
