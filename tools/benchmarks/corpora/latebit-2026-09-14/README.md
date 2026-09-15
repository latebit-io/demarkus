# Independent corpus: latebit, 2026-09-14, dataset v1

Frozen organizational knowledge from `mark://latebit`. The private GCS export archive stays in `tools/benchmarks/artifacts/`; this directory holds its canonical manifest and restored store plus dataset v1.

The pinned root contains 38 documents (37 active, one archived), 52 retained versions, and 81,163 compressed bytes. Restore reproduced every document fingerprint independent of exporter traversal order.

The cohort covers deployment facts, cross-project roles, graph contracts, supersession and contradictory historical guidance, routing aliases, immutable history, operational gaps, and conclusive absence. Expected `incomplete` outcomes are excluded: a healthy local frozen scope has no deterministic incomplete corpus answer. Failure and partial-scope behavior remains a harness control.

## Restore and inspect

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/latebit-2026-09-14/manifest.json -archive tools/benchmarks/artifacts/latebit-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/latebit-2026-09-14/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/latebit-2026-09-14/restored -questions tools/benchmarks/corpora/latebit-2026-09-14
```

`restored/` is ignored and shared by every dataset version for this corpus. A changed corpus creates a new corpus snapshot; changed tasks, rubric, scope, completion rule, scorer, or reader contract create a new dataset ID.
