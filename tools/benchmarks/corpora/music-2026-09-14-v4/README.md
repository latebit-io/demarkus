# Independent corpus: music, 2026-09-14, dataset v4

Frozen non-engineering knowledge from `mark://music`. This dataset reuses the unchanged v1 private GCS export archive in `tools/benchmarks/artifacts/`; Git holds its v4 manifest, eight scoped tasks, scorer-only rubric, and dataset identity.

The pinned root contains 277 active documents, 350 retained versions, and 168,275 compressed bytes. Restore reproduced every document fingerprint.

The cohort covers genre origins, multi-document lineage, artist influence, artist-to-work dependencies, contested attribution, immutable history, curatorial nuance, and conclusive absence. Human review approved 14 tasks across both frozen cohorts on 2026-09-15. Dataset v2 tightened the Chiptune absence query, and v3 adopted `independent-evidence-v2`. This v4 uses `section-first-outcome-v2`, whose explicit JSON schema includes the required outcome field.

No reader run or paid model call has been made for this dataset.

## Restore and inspect

```bash
make answer-bench
rm -rf tools/benchmarks/corpora/music-2026-09-14-v4/restored
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/music-2026-09-14-v4/manifest.json -archive tools/benchmarks/artifacts/music-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/music-2026-09-14-v4/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/music-2026-09-14-v4/restored -questions tools/benchmarks/corpora/music-2026-09-14-v4
```

`restored/` is ignored. A changed corpus, task, rubric, scope, completion rule, or reader contract requires a new dataset ID.
