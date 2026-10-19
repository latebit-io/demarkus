---
name: decision-number
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish]
---
/demarkus-memory:remember Record this decision: the CLI keeps the legacy parser behind a --legacy flag for one release, then removes it; the fixtures run through both parsers until then. Title it "Keep the legacy parser one release".
