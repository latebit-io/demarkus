# Source isolation: 2026-09-13

Branch `fix/graph-source-isolation`, base `3275da5`. Broker graph data, seed etags,
refresh timestamps and in-flight refreshes are tenant-scoped. Identity or backend
changes replace the whole scope. Tenant snapshots admit only owned source rows;
the knowledge profile retains organizational cross-world reads. The tenant gate
also checks document URLs before section-aware fetch/explore handlers strip anchors.

## Mechanical comparison

Against [the fixed baseline](baseline-2026-09-12/README.md), Apple M2, Go 1.26.4,
darwin/arm64, six samples, 200 ms, `-cpu 1`. No concurrent test/build/reader run
was launched during sampling; background system load and power mode were not
instrumented. Figures are synthetic handler sample medians, not production latency.

| Metric | Before | After |
|---|---:|---:|
| Backlinks leaked responses/op | 1 | **0** |
| Explore leaked responses/op | 1 | **0** |
| Missing own source/op, both handlers | 0 | **0** |
| Backlinks time | 2.831 µs | 2.484 µs (-12.26%) |
| Explore time | 37.74 µs | 39.93 µs (+5.78%) |
| Backlinks allocations/op | 35 | 28 |
| Explore allocations/op | 541 | 534 |
| Backlinks allocated bytes/op | 2,912 | 2,160 |
| Explore allocated bytes/op | 78.27 KiB | 77.68 KiB |

Pinned benchstat reports `p=0.002`, `n=6` for both handler timing differences.
Results contain one permitted source rather than two sources including a leak;
this is not an equal-output general graph speed comparison. Other packages also
show timing variation without changed implementations. Source isolation is the
acceptance criterion; the explore timing increase remains visible.

Context evidence recall, duplication, result tokens and budget overflow are
unchanged. Relative-target and silent-incomplete defects remain for subsequent
roadmap slices. Warm graph queries retain the same returned rows and allocations.

Raw samples and manifest remain local under ignored
`tools/benchmarks/artifacts/graph-after-isolation-2026-09-13/`; `status.txt` is
`complete`. Recorded tracked-diff hash: `e84c2be02f84f532d57f6174e42e8120e8918a76`.
Fixture, benchmark-source and dependency hashes match the before manifest.
The runner script hash differs because merged review fixes added the pre-output
untracked-input guard; benchmark invocations and parameters are unchanged.

## Frozen real-soul reader replay

[Metrics-only after report](../corpora/soul-2026-09-12/after-isolation.json), compared
with [section-first before](../corpora/soul-2026-09-12/baseline-section-first.json).
Strict comparison accepts the pinned corpus, questions, rubric, policy, model,
OpenCode version and `section-provenance-v2` scorer. No regrading or reader retries.

| Metric | Before | After |
|---|---:|---:|
| Correct supported outcomes | 16/16 | **16/16** |
| Total model tokens | 95,041 | 94,152 |
| Model tokens/correct outcome | 5,940.0625 | 5,884.5 (-0.94%) |
| Tool-result tokens | 12,364 | 12,364 |
| Model turns / MCP calls / protocol requests | 50 / 36 / 52 | 50 / 36 / 52 |

The replay exercises the local production server/client, not the broker tenant
boundary. It is a retrieval regression check, not evidence that isolation lowers
model cost. This small point-estimate token delta does not establish an improvement.
Private report/traces remain under ignored
`tools/benchmarks/artifacts/answers-after-isolation-2026-09-13/`.

## Verification

`make test`, targeted broker race tests and `bash pre-commit.sh` passed. Enforcing
regressions first reproduced both backlink/explore leaks and the foreign-section
fetch bypass. Coverage includes both tenant call orders, local crawls, legacy and
sharded snapshots, source title/label/anchor/count fidelity, spoofed foreign source
rows, revocation, identity/address/dial changes, refresh replacement, independent
tenants, late retired refresh completion and authorized organizational discovery.
