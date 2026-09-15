# Independent corpus: music, 2026-09-14, dataset v1

Frozen non-engineering knowledge from `mark://music`. The private GCS export archive stays in `tools/benchmarks/artifacts/`; this directory holds its canonical manifest and restored store plus dataset v1.

The pinned root contains 277 active documents, 350 retained versions, and 168,275 compressed bytes. Restore reproduced every document fingerprint.

The cohort covers genre origins, multi-document lineage, artist influence, artist-to-work dependencies, contested attribution, immutable history, curatorial nuance, and conclusive absence. Its vocabulary, document topology, prose, and relation patterns differ from the organizational corpus.

## Restore and inspect

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/music-2026-09-14/manifest.json -archive tools/benchmarks/artifacts/music-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/music-2026-09-14/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/music-2026-09-14/restored -questions tools/benchmarks/corpora/music-2026-09-14
```

`restored/` is ignored and shared by every dataset version for this corpus. A changed corpus creates a new corpus snapshot; changed tasks, rubric, scope, completion rule, scorer, or reader contract create a new dataset ID.
