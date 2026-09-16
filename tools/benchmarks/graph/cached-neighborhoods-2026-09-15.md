# Cached relation-aware neighborhoods benchmark

Mechanical before/after measurements for cached relation-aware neighborhoods and follow-up bounded-query optimizations. This report makes no answer-quality claim.

## Run

- Machine: Apple M2, Darwin arm64, Go 1.26.4.
- Samples: six per case, 200 ms, CPU 1.
- Before: `ede2b80243a1867aa438bf7962046a916e93a5fc`, with fixed version/etag added only to the broker fixture.
- After: `dd22c38ad6f2738e5bccf32b31dbd9fc78bb7956` plus tracked diff `6b8e50cb09ac96dfafae30dc59d8fea56e618304` recorded before this report was added.
- Raw local artifacts: `tools/benchmarks/artifacts/graph-before-neighborhoods-metadata-2026-09-15/` and `tools/benchmarks/artifacts/graph-after-neighborhoods-final4-2026-09-15/`.
- Both runs completed. Broker fixture and existing shared benchmark hashes match. New graphstore cases have after-only absolute measurements.

Measured after-source hashes: neighborhood `820c192449369fe5b643c16fa7d3abcd923d275b`, observation `0d380dc78565982c494f61d73489827d8832461e`, store `269022b673beff9d78534e5fccf1049973e9eee2`, client harness `057cfbf5e2ef48f9f1e74cbbb8956619358c8e30`, broker harness `878bdfb63dd15676c9e6228772e7a0827f65c497`.

```bash
bash scripts/graph-benchmark.sh tools/benchmarks/artifacts/graph-before-neighborhoods-metadata-2026-09-15
bash scripts/graph-benchmark.sh tools/benchmarks/artifacts/graph-after-neighborhoods-final4-2026-09-15
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da \
  tools/benchmarks/artifacts/graph-before-neighborhoods-metadata-2026-09-15/client.txt \
  tools/benchmarks/artifacts/graph-after-neighborhoods-final4-2026-09-15/client.txt
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da \
  tools/benchmarks/artifacts/graph-before-neighborhoods-metadata-2026-09-15/tools.txt \
  tools/benchmarks/artifacts/graph-after-neighborhoods-final4-2026-09-15/tools.txt
```

## Existing cases

| Case | Time | Bytes | Allocations | Correctness |
|---|---:|---:|---:|---|
| Backlinks, one matching row | -83.9% to -99.8% | unchanged | unchanged | same rows |
| Backlinks, 1,000 matching rows | -52.2% to -80.8% | -70.8% | -64.7% | same rows |
| Restore, 1,000 edges | no significant change | +0.8% | +0.09% | same result |
| Restore, 100,000 edges | +4.6% | -3.9% | +0.06% | same result |
| Broker `mark_explore` | +10.8% | +4.2% | +4.5% | zero leaks and zero missing own sources |
| Retrieval context geomean | no material change | unchanged | unchanged | recall, fetches and token counts unchanged |

Complete, cancelled and capped crawl cases had no significant timing change. Their work, bytes and allocations remain unchanged. Relative-target timing, bytes, allocations and correctness also remain unchanged.

## New cases

| Case | Median time | Bytes/op | Allocations/op |
|---|---:|---:|---:|
| Warm neighborhood, one row | 1.9-2.3 us | 3.4 KiB | 15 |
| Warm neighborhood, 1,000 matching rows, 100 returned | 171-182 us | 201.7 KiB | 240 |
| Restore plus neighborhood, 1,000 edges | 5.1 ms | 3.84 MiB | 17.2k |
| Restore plus neighborhood, 100,000 edges | 556 ms | 426.7 MiB | 1.70M |
| Stable observed representation, one link | 1.25 us | 952 B | 10 |
| Stable observed representation, 100 links | 4.39 us | 11.5 KiB | 10 |
| Changed observed representation, one link | 30.2 us | 70.5 KiB | 469 |
| Changed observed representation, 100 links | 395 us | 565.0 KiB | 2.5k |
| Save, 1,000 edges | 2.7 ms | 1.20 MiB | 1.0k |
| Save, 100,000 edges | 271 ms | 181.7 MiB | 100.1k |

Setup and cleanup assertions verify relation inclusion and exclusion, stable first/last row ordering, exact row totals, stable freshness advancement, changed revision/etag replacement, observed adjacency, and complete persisted edge counts outside timed loops. Focused unit tests verify equal-identity representation conflicts and stale-fingerprint rejection.

## Optimization gate

The first after run exposed avoidable costs. Broker `mark_explore` was +94.5% slower with +82.2% bytes and +79.8% allocations. A 1,000-neighbor query materialized all rows before returning 100, using about 1.13 MiB and 2,057 allocations. Restore added one backing allocation per indexed endpoint.

The final implementation pages before copying evidence, bulk-builds adjacency indexes, and refreshes byte-identical observations without reparsing Markdown or rebuilding topology. A SHA-256 fingerprint over body and relation metadata preserves equal-revision conflict detection. Fingerprints install only when the accepted node and adjacency still match, preventing concurrent observations from attaching stale content. The 1,000-neighbor query now uses about 202 KiB and 240 allocations. Broker `mark_explore` retains a measured 10.8% time and low single-digit memory cost for richer relation output and observation. Restore uses less memory at 100,000 edges with effectively unchanged allocation count and a measured 4.6% time cost.

## Paid cohort decision

No paid v4 after-arm cohort ran. Immutable v4 uses `scoped-direct-read-v1`, which exposes only fetch, list, lookup, and versions. It excludes `mark_explore`, so the neighborhood feature cannot affect reader behavior. Running it would measure provider variance rather than this slice; changing the tool profile would invalidate comparison with the v4 baseline.
