# Independent corpus: music, 2026-09-14, dataset v2

Frozen non-engineering knowledge from `mark://music`. This directory holds dataset-specific manifest, tasks, rubric, and identity while reusing the private v1 archive and canonical restored store.

The pinned root contains 277 active documents, 350 retained versions, and 168,275 compressed bytes. Restore reproduced every document fingerprint.

The cohort covers genre origins, multi-document lineage, artist influence, artist-to-work dependencies, contested attribution, immutable history, curatorial nuance, and conclusive absence. Human review approved 14 tasks across both frozen cohorts on 2026-09-15. This v2 changes the Chiptune absence completion query to target the missing entity directly.

## Restore and inspect

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/music-2026-09-14-v2/manifest.json -archive tools/benchmarks/artifacts/music-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/music-2026-09-14/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/music-2026-09-14/restored -questions tools/benchmarks/corpora/music-2026-09-14-v2
```

The canonical `restored/` store is shared by every dataset version. Changed dataset inputs require a new dataset ID; changed corpus bytes require a new corpus snapshot.
