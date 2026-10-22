---
name: publish-near-duplicate
runs: 2
max_turns: 12
timeout_seconds: 240
allowed_tools: [Skill, mcp__plugin_demarkus-memory_demarkus-memory__mark_fetch, mcp__plugin_demarkus-memory_demarkus-memory__mark_lookup, mcp__plugin_demarkus-memory_demarkus-memory__mark_list, mcp__plugin_demarkus-memory_demarkus-memory__mark_publish, mcp__plugin_demarkus-memory_demarkus-memory__mark_append]
---
Save this to my soul as a new document at /cwd/notes/shard-sizing.md, tagged store, shards. Body:

# Shard sizing

The bucket store shards by size rather than by count: a shard
growing past the limit is split at the next checkpoint, and one under the
floor is merged with its neighbour. Counting documents seemed simpler, but a
world of small notes and a world of long plans want different widths, and
the size bound keeps each shard fetch inside one round trip. The compactor
runs off the writer so a split never blocks a publish; the checkpoint row
carries the source of the write observation so replicas agree which
version a shard describes.

Tell me where it ended up and what the gate said, if anything.
