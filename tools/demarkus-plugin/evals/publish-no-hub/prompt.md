---
name: publish-no-hub
runs: 2
max_turns: 8
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_lookup, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish]
---
Save this to my soul as a new document at /cwd/notes/quic-retries.md: QUIC retries back off exponentially from 200ms and cap at 5s; a stream is abandoned after six retries. Tag it quic, retries.
