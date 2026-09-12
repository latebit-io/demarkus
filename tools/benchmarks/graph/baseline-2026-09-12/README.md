# Pre-roadmap baseline: 2026-09-12

23 cases, six samples each. Apple M2, Go 1.26.4, darwin/arm64, `-cpu 1`,
`-benchtime 200ms`. No concurrent agent-run tests during capture; power mode and
external machine load were not controlled. Timing is indicative local cost.

Production revision: `1524828c9ca3e2a3c07bd4bd4a24ef491cdaf67f`.
Benchmark branch: `test/graph-baseline`; harness and three test-helper signature
changes were uncommitted at capture. Production behavior was unchanged.

Raw evidence: [client.txt](client.txt), [tools.txt](tools.txt),
[manifest.txt](manifest.txt), [completion marker](status.txt).
Never overwrite these samples with an after run.

## Selected measurements

Latency values below are benchstat medians of six per-sample means, not p95.

| Case | Baseline |
|---|---:|
| Warm backlinks, 1k edges, one row | 2.552 µs/op |
| Warm backlinks, 10k edges, one row | 24.86 µs/op |
| Warm backlinks, 100k edges, one row | 248.7 µs/op |
| Warm backlinks, 100k edges, 1k rows | 367.5 µs/op |
| Fresh Store load + query, 100k edges, warm filesystem cache | 406.8 ms/op |
| Sparse backlink allocations, all three sizes | 312 B/op; 4 allocs/op |

## Quality gaps

- Tenant backlinks **and explore**: every measured response exposes Alice's
  source to Bob. Bob's own source is present. Targets: zero leaked responses,
  still zero missing own sources. This is a synthetic fixture, not live data.
- Relative targets: nested and parent-relative references are wrong in every
  iteration; root-relative and absolute cases resolve correctly.
- Pre-cancelled and ten-node-capped crawls return nil error without disclosing
  incompleteness. Complete crawl retains all 101 nodes and 100 edges.
- Wide nested context: 3/3 evidence passages, one duplicated passage, 143 result
  tokens. Tight 230-byte expansion budget: 2/3 evidence passages, one duplicated
  passage, 124 tokens; full table+expansion exceeds that budget by 202 bytes.
- Other context cases retain every required passage: direct 65 tokens,
  two-document 99, conflicting sources 98, failed-source-first 101.

Benchmark `PASS` reports successful measurement, not correct production behavior.
Scorer validation and existing tests pass; each production fix still needs an
enforcing regression test. This is a mechanical baseline, not LLM answer quality.

Rerun and compare using the [suite instructions](../README.md).
