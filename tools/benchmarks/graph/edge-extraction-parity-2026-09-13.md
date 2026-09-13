# Edge extraction parity: 2026-09-13

Branch `fix/graph-edge-resolution`, base `1dc2235`. Ordinary and federation
crawlers now use one graph-owned body/typed-edge extractor. Federation resolves
against the complete source document URL, discovers permitted typed-only foreign
targets, and retains its mark-only, loopback, and publish-only-hub policy.

## Mechanical comparison

Against [the fixed baseline](baseline-2026-09-12/README.md), Apple M2, Go 1.26.4,
darwin/arm64, six samples, 200 ms, `-cpu 1`. No concurrent test/build/model run
was launched during sampling. Background load and power mode were not measured.

| Metric | Before | After |
|---|---:|---:|
| Nested wrong targets/op | 1 | **0** |
| Parent-relative wrong targets/op | 1 | **0** |
| Root/absolute wrong targets/op | 0 / 0 | **0 / 0** |
| Relative-target time, geomean | 22.14 us | 22.87 us (+3.27%) |
| Relative-target allocated bytes, geomean | 65.01 KiB | 65.91 KiB (+1.40%) |
| Relative-target allocations, geomean | 429.2 | 439.5 (+2.39%) |
| Complete crawl nodes / edges / fetches | 101 / 100 / 101 | 101 / 100 / 101 |
| Complete crawl allocated bytes | 5.978 MiB | 6.007 MiB (+0.47%) |
| Complete crawl allocations | 39.87k | 40.07k (+0.51%) |

Pinned benchstat found no significant root or nested timing change. Parent and
absolute isolated cases increased 4.75% and 5.01% (`p=0.002`, `n=6`). Shared
extraction materializes normalized edge rows once per document; the measured
allocation increase is the cost. Full crawl output is unchanged.

Tenant disclosure counters remain zero after the prior isolation slice. Context
evidence recall, duplication, fetches, result tokens, and budget overflow are
unchanged. Silent cancelled/capped outcomes remain measured defects for the later
bounded-crawl slice.

Raw final samples and manifest remain local under ignored
`tools/benchmarks/artifacts/graph-edge-resolution-final-2026-09-13/`; status is
`complete`. Recorded implementation diff hash: `1898ce861d3431d5c1099143316f07a35cda455e`.
The pre-final extraction attempt remains under the separate ignored
`graph-edge-resolution-2026-09-13/` directory.

## Frozen real-soul reader replay

[Metrics-only after report](../corpora/soul-2026-09-12/after-edge-resolution.json)
compared strictly with the fixed section-first baseline. Corpus, model, OpenCode
version, reader policy, tasks, rubric, scorer, settings, and repeats match.

| Metric | Before | After |
|---|---:|---:|
| Correct supported outcomes | 16/16 | **16/16** |
| Total model tokens | 95,041 | 96,044 |
| Model tokens/correct outcome | 5,940.0625 | 6,002.75 (+1.06%) |
| Tool-result tokens | 12,364 | 12,327 (-0.30%) |
| Model turns / MCP calls / protocol requests | 50 / 36 / 52 | 51 / 36 / 52 |

No task regressed. The local production server/client replay does not exercise
federation edge extraction; the small model-token increase and result-token
decrease are point variation, not an attributed software effect. Raw model traces
remain local under ignored
`tools/benchmarks/artifacts/answers-edge-resolution-final-2026-09-13/`. The
pre-final replay remains in its separate ignored output directory.

## Verification

Enforcing tests reproduced nested and parent-relative failures before the fix.
Coverage now includes nested, parent-relative and absolute references, default
ports, typed relations, typed-only foreign discovery, title precedence, repeated
occurrences, external policies, and producer/consumer export parsing. Client and
broker graph race suites and `bash pre-commit.sh` pass.
