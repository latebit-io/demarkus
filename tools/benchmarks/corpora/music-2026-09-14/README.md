# Independent corpus: music, 2026-09-14

Frozen non-engineering knowledge from `mark://music`. The private GCS export archive stays in `tools/benchmarks/artifacts/`; Git holds its manifest, eight scoped tasks, scorer-only rubric, and dataset identity.

The pinned root contains 277 active documents, 350 retained versions, and 168,275 compressed bytes. Restore reproduced every document fingerprint.

The cohort covers genre origins, multi-document lineage, artist influence, artist-to-work dependencies, contested attribution, immutable history, curatorial nuance, and conclusive absence. Its vocabulary, document topology, prose, and relation patterns differ from the organizational corpus.

No reader run or paid model call has been made for this dataset.

## Restore and inspect

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/music-2026-09-14/manifest.json -archive tools/benchmarks/artifacts/music-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/music-2026-09-14/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/music-2026-09-14/restored -questions tools/benchmarks/corpora/music-2026-09-14
```

`restored/` is ignored. A changed corpus, task, rubric, scope, completion rule, or reader contract requires a new dataset ID.
