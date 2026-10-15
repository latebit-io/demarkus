---
name: publish-update
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish]
---
Fetch /cwd/roadmap.md from my soul and republish it with the bullet "- Next: ship link on publish." added at the end; keep everything else, tags included, as it is. Report the version it is at now.
