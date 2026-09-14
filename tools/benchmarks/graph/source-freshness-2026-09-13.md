# Revision-aware graph freshness

Revision-aware source observations were implemented on `feat/graph-source-freshness`,
based on clean `b70cf87`, and committed as `a925813`. PR review corrections below
remain uncommitted.
Design: [ADR 0013](../../../docs/adr/0013-graph-source-freshness.md),
[SPEC 12.2](../../../docs/SPEC.md#122-atomic-graph-snapshots), and the
[soul plan](mark://soul.demarkus.io/plans/graph-focus/source-freshness.md).

## Correctness and compatibility

- Source identity, document revision, raw-version etag, extraction view and
  observation state survive crawls, persistence, federation export and seed import.
- Newer comparable source revisions replace older adjacency; older observations
  cannot regress it. Equal conflicts and different-source comparisons are explicit.
- Metadata-only relations are refreshed without relying on body hashes.
- Complete empty sets and confirmed deletion remove obsolete edges. Same-revision
  archive/unarchive state is resolved by direct source observations.
- Failure, cancellation and caps preserve last-good evidence and occurrence counts.
  Failed reads do not convert seed ownership into local ownership.
- Overlapping owners retain independent candidates. A persisted source high-water
  mark keeps older surviving topology stale after a newer owner's withdrawal.
- Legacy snapshots remain unknown until valid revalidation. Known complete
  observations can supersede revisionless local absence.
- Ordinary and federation extraction views are explicit: a filtered equal-revision
  projection cannot manufacture an adjacency conflict with a document view.
- Disk schema v4 reads v1 through v3. Snapshot v2 readers accept v1 as unknown;
  old v1 readers reject v2 atomically. Legacy markdown tables remain intact, with
  an additive observation sidecar. The real federation golden is consumed by broker
  tests, including source identity/revision preservation through URL translation.

Focused fixtures, full client tests, client graph/store/fetch/federation/CLI/MCP/TUI
race suites, broker/retrieval race suites, producer/consumer contracts and
`bash pre-commit.sh` pass. No protocol verb, answer-ranking change, evidence-location
index or automatic graph-assisted retrieval was added.

## Direct revalidation cost

Source validation is capped at eight attempts, 8 MiB aggregate network/decoded
budgets, 128 KiB per-source rendered output and a fifteen-second caller-derived
deadline. It follows no edges; source attempts have a five-minute cooldown and
single-flight protection.

`BenchmarkSourceFreshnessRevalidation`, six samples, 200 ms, CPU 1:

| Measurement | Result |
|---|---:|
| Cold legacy source validation | 1 FETCH |
| Accepted decoded source payload | 51 bytes |
| Immediate repeat | 0 FETCHes |
| Median setup + validation + repeat | 27.6745 microseconds |
| Allocated bytes | 74,075 B/op |
| Allocations | 529/op |

The microbenchmark uses an in-memory fetcher: payload bytes are measured, network
latency is not. Enforcing fixtures separately show eleven failing sources handled
as eight, then three, then zero attempts; pre-fetch failures also respect the cap.
Cancellation, byte limits and concurrent calls retain last-good data.

## Fixed mechanical suite

Apple M2, darwin/arm64, Go 1.26.4; six samples, 200 ms, CPU 1. No benchmark workload,
fixture or dependency hash changed. These are per-sample means, not production p95.
Power mode was not separately recorded. Timings are point comparisons, with
unrelated context-assembly timing variation visible in the retained samples.

Final run: `tools/benchmarks/artifacts/graph-source-freshness-views-2026-09-13/`.
Status is complete; tracked implementation diff hash:
`1e241eacc01f6ea9125a35aeb47331d08cff5671`.
Comparison uses pinned `benchstat@v0.0.0-20260908200009-22c9c6c9d4da`
against `baseline-2026-09-12/`.

| Case | Baseline median | Current median | Change |
|---|---:|---:|---:|
| Backlinks, 1k edges / 1k rows | 91.82 us | 145.33 us | +58.28% |
| Backlinks, 100k edges / 1k rows | 367.5 us | 371.5 us | Not significant |
| Restore, 1k edges | 4.050 ms | 4.712 ms | +16.35% |
| Restore, 100k edges | 406.8 ms | 529.6 ms | +30.18% |
| Complete crawl | 2.227 ms | 2.280 ms | Not significant |
| Pre-cancelled crawl | 5.770 us | 0.9664 us | -83.25% |
| Capped crawl | 659.8 us | 486.4 us | -26.29% |
| Tenant backlinks | 2.831 us | 4.567 us | +61.32% |
| Tenant explore | 37.74 us | 42.62 us | +12.91% |

Source state and independent candidates have a measurable cost: 1k-row backlinks
allocate 384.1 KiB (+65.79%); 100k-edge restore allocates 444.2 MiB (+60.89%).
Complete crawl bytes rise 1.84%. First attempts exposed avoidable duplication and
sorting costs; selected local rows now serialize once, already-canonical stored
identities are not reparsed, graph output order is deterministic, and backlinks
copy observation metadata only when present. Remaining costs are retained here,
not hidden by a combined score.

Correctness counters stay intact: zero silent incomplete crawls, wrong relative
targets, tenant leaks and missing own sources. Complete crawl remains 101 nodes,
100 edges and 101 FETCHes; capped crawl retains 10 nodes, 100 boundary edges and
10 FETCHes; pre-cancelled crawl does no FETCHes. Deferred context overlap and
tight-budget defects retain their baseline measurements.

## Frozen ordinary-retrieval regression

Final valid run:
`tools/benchmarks/artifacts/answers-source-freshness-views-retry-2026-09-13/`.
Metrics-only report: [after-source-freshness.json](../corpora/soul-2026-09-12/after-source-freshness.json).

Corpus, model `openai/gpt-6-astra`, low variant, OpenCode 1.18.30, section-first
policy, eight steps, three-minute task timeout, two repeats, tasks, rubric and
`section-provenance-v2` scorer remained fixed. Server startup and the expected
52 protocol requests were verified. Strict comparison passes with complete usage.
This is a conditional ordinary-retrieval check, not process-owned release evidence:
the runner does not bind responses to the child process or verify its continued
liveness. Free-port and startup-log checks reduce accidental reuse but do not
establish that guarantee.

| Measurement | Baseline | Current |
|---|---:|---:|
| Correct | 16/16 | 16/16 |
| Model tokens | 95,041 | 95,683 |
| Tokens per correct | 5,940.0625 | 5,980.1875 (+0.6755%) |
| Tool-result tokens | 12,364 | 12,327 |
| Model turns | 50 | 51 |
| MCP calls | 36 | 36 |
| Protocol requests | 52 | 52 |

The real-soul proxy disables `mark_graph`; this reader uses lookup/fetch. Token
totals check ordinary retrieval and do not establish implicit-graph gains.
Graph-enabled answer evaluation remains separate.

## Attempt preservation and harness findings

All runs are local and ignored; archive and restored corpus remain unchanged.
Earlier mechanical directories use suffixes `source-freshness`,
`source-freshness-final`, `source-freshness-verified` and
`source-freshness-delivery`, each followed by `-2026-09-13`.

Earlier answer directories retain:

- `answers-source-freshness-2026-09-13`: outer harness timeout after seven reported
  successes; its fixture server became orphaned.
- `answers-source-freshness-final-2026-09-13`: invalid for final comparison. Its
  server failed to bind and the runner reused that orphan, recording zero requests.
  The owned orphan was identified and stopped; later runs verify the port first.
- `answers-source-freshness-verified-2026-09-13`: valid pre-final 16/16 replay,
  97,418 tokens; superseded by later implementation refinements.
- `answers-source-freshness-delivery-2026-09-13`: valid pre-view 16/16 replay,
  95,926 tokens; its metrics copy also stays in that ignored directory.
- `answers-source-freshness-views-2026-09-13`: OpenCode failed on attempt 15 with
  `Failed to execute statement`; 81,732 known tokens, incomplete usage, no valid
  cost ratio. The complete unchanged-cohort retry is the final comparison above.

The runner currently verifies an answering fixture, not ownership of the process
that answered. A bind failure can therefore yield an apparently successful replay.
This operational finding is recorded without changing the frozen runner or scorer.
Authoritative process-owned evidence would require per-run endpoint or authentication
nonce validation, child ownership/liveness checks, and a separate regenerated run.
The mechanical runner's untracked-input guard blocked the first run; the user
explicitly authorized staging the nine new implementation, test and ADR files.

## PR review corrections

[PR 451](https://github.com/latebit-io/demarkus/pull/451) feedback was checked against
`a925813`. Existing value-range cleanup remains included. Seven comments are
addressed locally:

| Finding | Correction |
|---|---|
| Failed seed diagnostics vanished after restart | Save immediately after failure marking; log save errors |
| Tool descriptions exceeded three sentences | Shorten both variants while preserving URL hints |
| Observation marshaling error lacked diagnostics | Log the actual error and retain legacy fallback output |
| Heading text in titles rejected valid exports | Detect a complete header line; still reject malformed real sections |
| Revalidation test could wait forever | Bound startup and cancellation-completion waits |
| Older federation revisions lost attempt state | Advance attempt time and record regression while retaining last-good fields |
| Replay implied process-owned evidence | Make conditional attribution explicit; frozen runner and scorer unchanged |

Regressions reproduced the parser, logging and persisted-state defects before the
fixes. Round trips now retain seed-failure diagnostics and older-revision attempt
state without losing source revision, etag, content hash or outgoing evidence.
Failed seed refreshes now incur a disk save; the in-memory validation benchmark
does not measure that filesystem path.
Full client tests, producer/consumer contracts, affected client races,
broker/retrieval races and `bash pre-commit.sh` pass. The generic docstring-coverage
warning remains subordinate to the repository's terse-comment policy.

Mechanical rerun: `tools/benchmarks/artifacts/graph-source-freshness-review-2026-09-14/`,
complete; working-tree diff hash `5e5c76336ebebfdf51d324377a993ece5c733f31`.
Six samples, 200 ms, CPU 1; same fixed harness and dependency hashes. Correctness
counters remain zero for silent incomplete outcomes, wrong targets, tenant leaks
and missing own sources. Complete/capped/pre-cancelled work counters remain
101/100/101, 10/100/10 and 0/0/0 nodes/edges/FETCHes respectively.

Against the fixed pre-roadmap baseline: complete crawl median 2.475 ms (+11.13%),
100k-edge restore 553.7 ms (+36.11%), tenant backlinks 4.444 us (+56.96%), tenant
explore 40.62 us (+7.62%). Unchanged paths also vary; these movements are not
attributed solely to the review fixes. Allocation costs remain as previously
recorded. Direct validation still uses one FETCH/51 decoded bytes and zero repeat
FETCHes: median setup/validation/repeat 28.3035 us, 74,075 B/op, 529 allocations.

Frozen rerun: `tools/benchmarks/artifacts/answers-source-freshness-review-2026-09-14/`;
metrics-only [after-source-freshness-review.json](../corpora/soul-2026-09-12/after-source-freshness-review.json).
Result: 16/16 correct, complete usage, 93,815 model tokens, 5,863.4375 per correct
(-1.2900% against the fixed baseline), 12,364 tool-result tokens, 50 turns,
36 MCP calls and 52 protocol requests. Strict comparison passes. This is still
conditional ordinary-retrieval regression data, not process-owned release evidence
or a graph-assisted gain. Corpus, model, policy, tasks, rubric and scorer are fixed;
all prior reports, archive and raw traces remain preserved.
