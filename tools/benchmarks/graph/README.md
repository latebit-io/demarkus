# Graph before/after benchmark

Recorded baseline: [2026-09-12](baseline-2026-09-12/README.md).

Run **before the first roadmap fix**, then after each slice, on the same machine:

```bash
bash scripts/graph-benchmark.sh /tmp/graph-before
bash scripts/graph-benchmark.sh /tmp/graph-after
benchstat /tmp/graph-before/client.txt /tmp/graph-after/client.txt
benchstat /tmp/graph-before/tools.txt /tmp/graph-after/tools.txt
```

Commands run from the repository root. Each output directory must be new, with
an existing parent. A run is valid only when `status.txt` says `complete`.
`benchstat` is the optional OSS comparison tool from `golang.org/x/perf/cmd/benchstat`.
The raw Go benchmark files remain readable without it.

Pinned installation: `go install golang.org/x/perf/cmd/benchstat@v0.0.0-20260908200009-22c9c6c9d4da`.
Alternatively use `go run` with that same package/version and the two report paths.

Defaults: six repeated samples per case, 200 ms per sample, `-cpu 1`.
`BENCH_COUNT` and `BENCH_TIME` override these for both runs. Compare the same
settings, Go version, architecture, fixture and harness hashes in `manifest.txt`.
Record hardware, power mode and competing load; do not interpret cross-machine
latency deltas as improvements. Each report includes the source revision and
working-tree status because benchmark-only changes may not yet be committed.

## Measurements

| Case | Measures | Better |
|---|---|---|
| Backlinks, 1k/10k/100k edges, fan-in 1/1k | Warm local query ns/op, B/op, allocs/op; fixed returned rows | Lower cost, same rows |
| Restore, 1k/100k edges | Fresh Store load plus query; warm OS file cache | Lower cost |
| Complete, cancelled, capped crawl | Fetches, nodes, edges, silent incomplete outcomes | No silent incomplete outcomes; complete graph preserved |
| Relative targets | Root, nested, parent-relative, absolute link resolution | Zero wrong targets |
| Tenant backlinks and explore | Alice source visibility to Bob, alongside Bob's permitted source | Zero leaked responses AND zero missing own sources |
| Context, six frozen cases | Exact source evidence recall, duplicated evidence, full table+expansion tokens, fetches, bytes over budget | Preserve recall, reduce duplication/cost |

Known defects are **measurements**, not expected behavior pinned by passing tests.
A benchmark `PASS` means the measurement ran, not that its quality metrics are
acceptable. Fixes must add enforcing regression tests. Never trade isolation or
evidence coverage for speed. Retain every sample and category; do not blend these
dimensions into one score. Use repeated samples and benchstat uncertainty rather
than treating a single small latency delta as progress. These are per-sample mean
latencies, not production p95 or network measurements.

## Frozen retrieval fixture

`tools/internal/retrievalbench/questions/graph-context-v1.json` fixes source bodies,
ranked input rows, query and budget. It tests **context assembly given candidates**,
not search ranking or an agent. Expected source passages go only to the scorer
after retrieval. A hit requires exact passage text in a source-framed block;
table snippets, titles and paths alone never count. Overlap counts repeated
expected passages, not all possible repeated prose.

Timing excludes scoring/tokenization. Token counts use `o200k_base`, covering the
entire table, expansion frames and notes. They exclude MCP JSON, tool schemas,
prompts and model output. `over-budget-B/op` measures table+expansion against the
requested byte budget; today's API promises only an expansion budget, so this is
a full-result efficiency gap, not a wire-contract failure. The tight-budget case
intentionally exposes lost evidence; preserve its budget when comparing changes.

No live tenant documents, network services, credentials or model API are used.
The baseline measures current production functions with synthetic data.
Tenant timings include the handler and fake-dispatch bookkeeping, not transport.
Their two-source fixture is not an exhaustive isolation audit.

## Relevance gate still required

This suite establishes reproducible correctness and mechanical cost baselines.
The [model-driven answer suite](../answers/README.md) now adds full provider token
accounting and independent scoring on a frozen synthetic cohort. Before broad
relevance claims, extend evaluation to held-out questions over real-world corpora.
Keep model, sources and rubric fixed. The existing
`demarkus-retrieval-bench` lookup/fetch strategy is oracle-assisted and cannot
substitute for that evaluation.

Design and delivery: [evaluation](mark://soul.demarkus.io/plans/graph-focus/evaluation.md),
[roadmap](mark://soul.demarkus.io/roadmap/graph-focus.md).
