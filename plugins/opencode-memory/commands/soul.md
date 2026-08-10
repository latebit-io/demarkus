---
description: Show the local demarkus soul index
---

# /soul

Use the `mark_fetch` MCP tool to fetch `/index.md` from the demarkus-memory server and render the full contents to the user. Only a `not-found` status means the index is absent — say so and suggest populating it. On any other error, surface the failure verbatim and stop (suggest `/soul-status` to diagnose); do not suggest populating.
