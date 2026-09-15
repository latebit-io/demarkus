# Independent corpus: music, 2026-09-14, dataset v2

Frozen non-engineering knowledge from `mark://music`. This dataset reuses the unchanged v1 private GCS export archive in `tools/benchmarks/artifacts/`; Git holds its v2 manifest, eight scoped tasks, scorer-only rubric, and dataset identity.

The pinned root contains 277 active documents, 350 retained versions, and 168,275 compressed bytes. Restore reproduced every document fingerprint.

The cohort covers genre origins, multi-document lineage, artist influence, artist-to-work dependencies, contested attribution, immutable history, curatorial nuance, and conclusive absence. Human review approved 14 tasks across both frozen cohorts on 2026-09-15. This v2 changes the Chiptune absence completion query to target the missing entity directly.

No reader run or paid model call has been made for this dataset.

## Restore and inspect

```bash
make answer-bench
rm -rf tools/benchmarks/corpora/music-2026-09-14-v2/restored
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/music-2026-09-14-v2/manifest.json -archive tools/benchmarks/artifacts/music-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/music-2026-09-14-v2/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/music-2026-09-14-v2/restored -questions tools/benchmarks/corpora/music-2026-09-14-v2
```

`restored/` is ignored. A changed corpus, task, rubric, scope, completion rule, or reader contract requires a new dataset ID.
