# Real soul baseline: 2026-09-12

This preserves the original budget-body reader. Use the newer
[section-first comparison](section-first-comparison-2026-09-12.md) and its
common-rubric report as the baseline for future software changes.

Actual `soul.demarkus.io` corpus, copied directly over SSH with rsync from its
live versioned store. A checksum-only second pass reported no differences.
The snapshot contains **311 active documents and 2,184 stored versions**.
No document bodies or version numbers were synthesized, renumbered, or rewritten.

A local production `demarkus-server` serves this frozen copy read-only. The
production MCP client keeps `mark://soul.demarkus.io` as its logical identity and
routes transport to the local server using `-dial-address`. This measures real
content and production retrieval, not the remote host's network/server latency.

## Recorded baseline

| Metric | Value |
|---|---:|
| Real-source questions | 8 |
| Fresh sessions per question | 2 |
| Correct, cited answers | 16/16 |
| Total model tokens | 210,381 |
| Tokens per correct answer | 13,148.8125 |
| Uncached input tokens | 120,273 |
| Cache-read input tokens | 86,656 |
| Visible output tokens | 3,452 |
| Reported reasoning / cache-write tokens | 0 / 0 |
| Model turns | 51 |
| MCP calls | 36 |
| Internal protocol requests | 178 |

Model: `openai/gpt-6-astra`, variant `low`; OpenCode 1.18.30, eight-step limit,
three-minute timeout. Questions cover protocol constants, default document type,
licensing, ADR supersession across two documents, LOOKUP response semantics,
OAuth redirect policy, server roles, and a historical ADR revision. They are
source-grounded questions authored before the run, not a held-out statistical
evaluation of every use case. Named-document questions make this a scoped recall
cohort. Preserve accuracy while reducing total tokens; repeat paired runs before
claiming significance.

All responses passed the rubric as frozen before model execution. An offline
recheck produced identical scores and spend; no grading correction was required.
The corpus fingerprint was identical before and after the run:

`sha256-b152758c5800ba293988417c52cf8f5044ec4e96e3c41099045cbe05ddd4fe4e`

Tasks: `sha256-f0fb85b07672b1088df85ff6c2f1741eea23533b194e450bcdf54f1199715e55`.
Rubric: `sha256-838d899d9a8b2c4ee0d6a3ba3685074f11a2a750345d75afdfded0d65b0d6a26`.

## Reproduce and compare

Private data lives under ignored `tools/benchmarks/answers/soul-local/2026-09-12/`:

- `corpus/`: exact versioned store copy, including history and symlinks.
- `tasks.json`, `rubric.json`: reader questions and separately held scoring keys.
- `baseline/report.json`: authoritative before report, including per-task results.
- `baseline/*-events.jsonl`: raw provider usage and tool traces.
- `baseline/recheck.json`: offline replay of grading, same results and spend.

From the repo root:

```bash
make answer-bench
tools/bin/demarkus-answer-bench inspect -corpus tools/benchmarks/answers/soul-local/2026-09-12/corpus -questions tools/benchmarks/answers/soul-local/2026-09-12
tools/bin/demarkus-answer-bench -reader-policy budget-body -corpus tools/benchmarks/answers/soul-local/2026-09-12/corpus -questions tools/benchmarks/answers/soul-local/2026-09-12 -origin mark://soul.demarkus.io -out /tmp/soul-after
tools/bin/demarkus-answer-bench compare tools/benchmarks/answers/soul-local/2026-09-12/baseline/report.json /tmp/soul-after/report.json
```

Keep the same snapshot for after runs. Re-copying the changing live corpus creates
a different experiment. The comparator rejects changed corpus, tasks, rubric,
model or settings. Real-soul and synthetic results cannot be compared as a gain
or regression.

Explicit `mark_graph` crawling is omitted from this copied-corpus reader because
it can follow foreign authorities outside the snapshot. Cached backlinks/explore,
lookup, fetch, list, versions and discovery retain production behavior. The
separate [mechanical graph suite](../graph/README.md) still exercises crawling.
Promoted documents outside soul remain out of scope rather than being fetched
from another live world. This boundary is fixed across before/after runs.

Full tests, copied-history/fingerprint checks, endpoint-routing tests, live model
capture, regrading parity, and pre-commit passed. Code and summary are uncommitted
on `test/soul-answer-baseline`; source history remains in the private snapshot.

[Runner/accounting details](README.md).
