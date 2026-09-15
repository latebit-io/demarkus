# Independent corpus: latebit, 2026-09-14, dataset v2

Frozen organizational knowledge from `mark://latebit`. This dataset reuses the unchanged v1 private GCS export archive in `tools/benchmarks/artifacts/`; Git holds its v2 manifest, eight scoped tasks, scorer-only rubric, and dataset identity.

The pinned root contains 38 documents (37 active, one archived), 52 retained versions, and 81,163 compressed bytes. Restore reproduced every document fingerprint independent of exporter traversal order.

The cohort covers deployment facts, cross-project roles, graph contracts, supersession and contradictory historical guidance, routing aliases, immutable history, operational gaps, and conclusive absence. Human review approved 14 tasks across both frozen cohorts on 2026-09-15. This v2 changes the ADR 0009 absence completion query to target the missing entity directly.

No reader run or paid model call has been made for this dataset.

## Restore and inspect

```bash
make answer-bench
rm -rf tools/benchmarks/corpora/latebit-2026-09-14-v2/restored
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/latebit-2026-09-14-v2/manifest.json -archive tools/benchmarks/artifacts/latebit-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/latebit-2026-09-14-v2/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/latebit-2026-09-14-v2/restored -questions tools/benchmarks/corpora/latebit-2026-09-14-v2
```

`restored/` is ignored. A changed corpus, task, rubric, scope, completion rule, or reader contract requires a new dataset ID.
