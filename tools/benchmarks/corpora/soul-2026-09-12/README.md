# Standard corpus: demarkus soul, 2026-09-12

Git holds this definition, `manifest.json`, questions, rubric, and metrics-only
before reports. Corpus bytes and raw model traces stay outside Git. The user
selected local archive storage for now; no remote upload or public release asset
is configured. A fresh checkout needs the pinned private archive supplied by its
owner, not a fresh download of the changing live soul.

Archive: **11,340,615 bytes (11.3 MB)**; 320 versioned documents (311 active,
9 archived), 2,184 versions. Packing and restoring reproduced the original corpus
fingerprint exactly. `baseline-section-first.json` is the software-change before
report; `baseline-budget-body.json` preserves the earlier reader-policy arm.

The manifest pins compressed bytes and the logical corpus fingerprint, including
all versioned documents, retained history, publisher metadata, archive state and
modified times. The archive is gzip-compressed JSONL produced through the existing
store export contract. Restore uses the store import contract to recreate version
files and symlinks; it refuses an existing destination and verifies the resulting
fingerprint. Unversioned files that the server does not serve are not corpus data.

## Restore

From repo root, put the private archive in ignored `tools/benchmarks/artifacts/`:

```bash
make answer-bench
tools/bin/demarkus-answer-bench corpus-restore -manifest tools/benchmarks/corpora/soul-2026-09-12/manifest.json -archive tools/benchmarks/artifacts/soul-2026-09-12-v1.jsonl.gz -root tools/benchmarks/corpora/soul-2026-09-12/restored
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/corpora/soul-2026-09-12/restored -questions tools/benchmarks/corpora/soul-2026-09-12
```

`restored/` is ignored. Both archive checksum and reconstructed corpus fingerprint
must pass. Retain the archive: its contents are independent of future changes on
`soul.demarkus.io`.

## Benchmark and compare

```bash
tools/bin/demarkus-answer-bench -reader-policy section-first -corpus tools/benchmarks/corpora/soul-2026-09-12/restored -questions tools/benchmarks/corpora/soul-2026-09-12 -origin mark://soul.demarkus.io -out tools/benchmarks/artifacts/after
tools/bin/demarkus-answer-bench compare tools/benchmarks/corpora/soul-2026-09-12/baseline-section-first.json tools/benchmarks/artifacts/after/report.json
```

Keep model, policy, questions and rubric unchanged when comparing software.
Metrics-only baselines retain token buckets, calls, grades, corpus/policy hashes
and the private source report's digest. They omit retrieved text, answers,
arguments, session IDs and diagnostics, so they cannot be rescored. Regrading
requires original private traces and must be applied to both comparison arms.

Questions and rubric are the common v2 definitions from the reader-policy study;
source snippets in the rubric support those eight scoring rules, not a copy of
the corpus. The model never receives the rubric. See the [policy comparison](../../answers/section-first-comparison-2026-09-12.md)
for the original grading correction and measurement boundaries.

## Rebuild an artifact deliberately

Use new output filenames; pack never overwrites an artifact or manifest:

```bash
tools/bin/demarkus-answer-bench corpus-pack -root PATH_TO_VERSIONED_STORE -id NEW_CORPUS_ID -source mark://soul.demarkus.io -archive tools/benchmarks/artifacts/NEW_CORPUS_ID.jsonl.gz -manifest NEW_MANIFEST.json
```

A changed corpus is a new benchmark dataset. Never repoint this manifest at a
different snapshot and call it the same before/after experiment. Compression
does not make corpus content appropriate for Git.
