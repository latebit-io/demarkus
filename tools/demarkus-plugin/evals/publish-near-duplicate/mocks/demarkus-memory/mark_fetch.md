status: ok
version: 1
tags: debugging,shards
type: Reference

# Debugging

Lessons from bugs and investigations.

## Shards

The bucket store shards by size, not by count: a shard that
grows past the limit splits on the next checkpoint, and a shard under the
floor merges with its neighbour. Counting documents looked simpler but a
world of small notes and a world of long plans need different widths, and
the size bound keeps every shard fetch under one round trip. The compactor
runs off the writer so a split never blocks a publish; the checkpoint row
carries the source of the write observation so replicas agree on which
version a shard describes.
