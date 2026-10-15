---
name: archive-gate
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_archive]
---
/demarkus-memory:soul-archive /cwd/old.md
