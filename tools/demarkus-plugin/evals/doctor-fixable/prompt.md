---
name: doctor-fixable
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_doctor, mcp__plugin_demarkus-memory_demarkus-memory__mark_curate, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish]
---
/demarkus-memory:soul-doctor cwd
