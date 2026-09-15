# Independent corpus: latebit, 2026-09-14, dataset v2

Frozen organizational knowledge from `mark://latebit`. This directory holds dataset-specific manifest, tasks, rubric, and identity while reusing the private v1 archive and canonical restored store.

The pinned root contains 38 documents (37 active, one archived), 52 retained versions, and 81,163 compressed bytes. Restore reproduced every document fingerprint independent of exporter traversal order.

The cohort covers deployment facts, cross-project roles, graph contracts, supersession and contradictory historical guidance, routing aliases, immutable history, operational gaps, and conclusive absence. Human review approved 14 tasks across both frozen cohorts on 2026-09-15. This v2 changes the ADR 0009 absence completion query to target the missing entity directly.

## Restore and inspect

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/latebit-2026-09-14-v2/manifest.json -archive tools/benchmarks/artifacts/latebit-2026-09-14-v1.jsonl.gz -root tools/benchmarks/corpora/latebit-2026-09-14/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/latebit-2026-09-14/restored -questions tools/benchmarks/corpora/latebit-2026-09-14-v2
```

The canonical `restored/` store is shared by every dataset version. Changed dataset inputs require a new dataset ID; changed corpus bytes require a new corpus snapshot.
