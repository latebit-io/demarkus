---
name: journal
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_append, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch]
---
/demarkus-memory:soul-journal Pruned the remember skill to the harness-specific parts; the route now refuses an archived project.
