# Bounded crawl outcomes: 2026-09-13

Branch `fix/graph-crawl-outcomes`, base `d94be35`. Level-ordered batches fix
first-arrival depth suppression. Capped, cancelled and failed crawls return
observations plus an explicit incomplete outcome. Completeness means only the
requested neighborhood; stored graphs have unknown traversal coverage.

## Correctness and bounds

The competing-path regression failed five times before implementation: a shared
node was recorded at depth 3 instead of 2, omitting its valid depth-3 descendant.
Level barriers prevent longer paths overtaking shorter ones; workers are joined
before the next batch. No goroutine is created for a queued child.

Default bounds: 1,000 admitted nodes, frontier capped at the node budget across
both BFS levels, five workers with a hard ceiling of 32, 64 MiB of network
response reads and 64 MiB of accepted decoded payloads, and 1 MiB of rendered
graph output. MCP, broker and TUI retain their 200-node setting. Output budgets
have a 1,024-byte minimum; envelope and row reservations count toward the limit.
Network reads include failed attempts and retries. Cache responses count toward
the decoded budget. Outstanding read reservations and output estimates may
conservatively stop before every budget byte is used.

An exact-fit node cap is complete if no required target was excluded. Boundary
edges remain visible without fetching their targets. A truncated source never
replaces a stored complete row, etag, edge set or occurrence count; newly observed
edges can still be retained. Failed protocol statuses cannot erase seeds.
MCP and broker share a summary renderer; TUI retains partial graphs and cancels
superseded/closed crawls. Existing federation complete-generation gates and
producer/consumer formats pass unchanged.

Tests cover competing depths, pre-cancellation, blocked network cancellation,
5,000-target fan-out, admission/frontier/worker caps, byte/output limits, exact-fit
and depth boundaries, failed peers, persisted partial sources, adapter outcomes
and stale TUI results. The expanded race run exposed an existing unsynchronized
capture in `fetch/lookup_match_test.go`; a channel now synchronizes that fixture.

## Mechanical comparison

Fixed [baseline](baseline-2026-09-12/README.md), Apple M2, Go 1.26.4,
darwin/arm64, six samples, 200 ms, `-cpu 1`, pinned benchstat. No concurrent
agent-run test, build or model workload during sampling. External load and power
mode were not measured. These are per-sample mean latencies, not production p95.

| Metric | Before | After |
|---|---:|---:|
| Pre-cancelled silent-incomplete/op | 1 | **0** |
| Node-capped silent-incomplete/op | 1 | **0** |
| Complete nodes / edges / fetches | 101 / 100 / 101 | **101 / 100 / 101** |
| Capped nodes / edges / fetches | 10 / 100 / 10 | **10 / 100 / 10** |
| Pre-cancelled nodes / edges / fetches | 0 / 0 / 0 | **0 / 0 / 0** |
| Complete time | 2.227 ms | 2.525 ms (+13.38%) |
| Complete allocated bytes | 5.978 MiB | 6.047 MiB (+1.15%) |
| Complete allocations | 39.87k | 41.69k (+4.56%) |
| Pre-cancelled time | 5.770 us | 0.636 us (-88.98%) |
| Pre-cancelled allocated bytes | 25,764.5 | 696 (-97.30%) |
| Capped time | 659.8 us | 550.9 us (-16.51%) |
| Capped allocated bytes | 975.7 KiB | 976.5 KiB (+0.08%) |
| Capped allocations | 6.101k | 6.905k (+13.18%) |

Crawl timing changes above have `p=0.002`, `n=6`. Output accounting and batch
bookkeeping add allocations; level barriers can reduce network concurrency
behind slow peers. The fixture uses one worker and does not measure that network
tradeoff. Unchanged context cases also slowed (geomean +8.81%), so the entire
timing delta cannot be cleanly attributed to this implementation.

Extraction wrong-targets and tenant disclosure counters remain zero after the
earlier slices; own-source counters remain zero. Context recall, duplication,
fetches, result tokens, byte overflow and allocations remain unchanged.
Relative-target geomean time is +19.09% versus the pre-roadmap baseline; its
allocated bytes are +1.47% and allocations +2.39%. No relevance claim follows.

The mechanical fixture, workload and counters are unchanged. Its fetch function
now accepts context and its expected-error guard accepts `graph.ErrIncomplete`,
so capped outcomes can be measured instead of terminating the runner. The silent
outcome counter still measures nil errors exactly as before.

Raw samples and complete manifest remain ignored/local in
`tools/benchmarks/artifacts/graph-crawl-outcomes-2026-09-13/`.
Implementation diff hash: `ebe8284d0bafd2d5df0406a90d7b5bdb894aa228`.
The initial runner invocation rejected eight untracked Go inputs before creating
output. The user then authorized staging those eight files only. Earlier runs,
including prior slices' pre-final attempts, remain untouched.

## Frozen real-soul replay

[Metrics-only report](../corpora/soul-2026-09-12/after-crawl-outcomes.json).
Strict comparison with `baseline-section-first.json` passed: corpus, model,
variant, OpenCode version, reader policy, tasks, rubric, scorer and settings match.

| Metric | Before | After |
|---|---:|---:|
| Correct supported outcomes | 16/16 | **16/16** |
| Total model tokens | 95,041 | 94,259 |
| Model tokens/correct outcome | 5,940.0625 | 5,891.1875 (-0.82%) |
| Tool-result tokens | 12,364 | 12,364 |
| Turns / MCP calls / protocol requests | 50 / 36 / 52 | 50 / 36 / 52 |

Usage accounting is complete; no task regressed. The reader made lookup/fetch
calls, not graph crawls. Model-token variation is not attributed to this slice.
Raw traces remain ignored/local in
`tools/benchmarks/artifacts/answers-crawl-outcomes-2026-09-13/`.
Archive and restored corpus remain local and ignored.

## Verification

Client suite, broker and retrieval benchmark suites, graph producer/consumer
contracts, client graph/store/fetch/federation/adapter race suites, broker graph
race suite and `bash pre-commit.sh` passed. Review checked level admission,
resource accounting, cancellation propagation, source replacement, output scope,
TUI stale results and federation publication gates. Initial compile/lint failures
and the test-fixture race were corrected before measurement.

## Review checkpoint

PR 449 review fixes build on user commit `af6efac`. Oversized summaries now
retain a minimal incomplete node when it fits; roots whose minimal row cannot
fit are rejected before fetching. External roots accept a nil fetch function.
Joined persistence warnings remove the already-rendered outcome through a shared
helper, retaining other errors across CLI, MCP, broker and TUI. Partial TUI nodes
use a distinct icon.

Regressions reproduced root loss, a fetch for an unrepresentable root, external
root rejection and duplicated TUI outcome text. Tests also cover child-depth
retention and nested joined warnings. Affected client and broker race suites,
federation producer/consumer contracts and `bash pre-commit.sh` pass.

New mechanical run: `tools/benchmarks/artifacts/graph-crawl-outcomes-review-2026-09-13/`;
diff hash `f8de99e2c1cbf10cec4af057df31e520181a4ed8`. Settings and fixtures remain
fixed; no concurrent agent-run workload during sampling. Correctness counters
are unchanged: no silent partial outcomes, complete crawl 101/100/101
nodes/edges/fetches. Against the fixed baseline, complete time is 2.361 ms
(+6.04%), bytes 6.048 MiB (+1.16%), allocations +4.57%; pre-cancelled time
0.9265 us (-83.94%), 904 bytes; capped time 480.4 us (-27.19%), 976.7 KiB
(+0.10%). Minimum-row validation adds five allocations per crawl. Timing
variation between runs is not attributed solely to these review changes.

[Review replay metrics](../corpora/soul-2026-09-12/after-crawl-outcomes-review.json):
16/16 correct, 96,193 model tokens, 6,012.0625 per correct (+1.21% versus fixed
baseline), 12,395 result tokens, 51 turns, 36 MCP calls and 52 protocol requests.
Strict comparison passed with complete usage and no task regression. The reader
still makes no graph calls; token variation is not attributed. Raw traces remain
local under `tools/benchmarks/artifacts/answers-crawl-outcomes-review-2026-09-13/`.
All earlier runs and metrics remain preserved.
