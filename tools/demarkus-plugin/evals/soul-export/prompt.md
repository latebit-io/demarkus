---
name: soul-export
runs: 2
max_turns: 14
timeout_seconds: 300
env:
  EVAL_PROJECT: new
allowed_tools: [Skill, Write, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch]
---
/demarkus-memory:soul-export export
