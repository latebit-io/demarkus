# End-to-end baseline: 2026-09-12

Historical synthetic calibration. Referenced raw reports and traces are retained
locally and ignored by Git; the standard real-corpus metrics are versioned under
`tools/benchmarks/corpora/soul-2026-09-12/`.

Comparison baseline: **[report-corrected.json](report-corrected.json)**.

Eight questions, two fresh reader sessions each, over ten frozen synthetic
documents. Real production server and MCP binaries from revision
`1524828c9ca3e2a3c07bd4bd4a24ef491cdaf67f`, with uncommitted benchmark additions on
`test/graph-answer-baseline`. No production graph/retrieval fixes were applied.
Model: `openai/gpt-6-astra`, variant `low`; OpenCode 1.18.30.

## Results

| Metric | Baseline |
|---|---:|
| Correct, source-supported outcomes | 16/16 |
| Answerable outcomes | 14/14 |
| Correct abstentions | 2/2 |
| All model tokens | 83,130 |
| Tokens per correct outcome | 5,195.625 |
| Uncached input | 46,265 |
| Cache-read input | 34,560 |
| Cache-write input | 0 |
| Visible output | 2,305 |
| Provider-reported reasoning | 0 |
| Model turns | 50 |
| MCP tool calls | 38 |
| Internal protocol requests | 90 |

The token numerator includes every model turn's prompts, schemas, tool interaction
and replayed history. Cached tokens remain counted. Zero reported reasoning means
the provider reported zero for these calls; the harness does not estimate hidden
tokens or dollar charges. Each category's counts and spend are in the report.

This small cohort reaches an accuracy ceiling. It can measure token savings while
preserving these outcomes; it cannot establish broad real-world accuracy gains.
Two repeats are an initial observation, not a statistical confidence bound.

## Calibration correction

The untouched [original report](report.json) marked 14/16 correct. Review of both
multi-document answers found correct step values and complete source support,
including the dependency handoff citation. The original rubric had omitted that
valid supporting passage and consequently rejected it as an unsupported extra.

The frozen rubric now requires the complete evidence chain: first step definition,
handoff to the audit procedure, and audit step definition. A regression test checks
that the dependency provenance is required. Expected answer values, questions,
source documents, model inputs and provider usage were unchanged.

`report-corrected.json` regrades the exact original traces, records the original
report's SHA-256, and carries the corrected rubric hash. **14/16 to 16/16 is a
grading correction, not an optimization result.** Both reports and all raw events
are retained; no extra model calls were made for rescoring.

## After a roadmap slice

From the repository root:

```bash
make answer-bench
tools/bin/demarkus-answer-bench -reader-policy budget-body -out /tmp/answers-after
tools/bin/demarkus-answer-bench compare tools/benchmarks/answers/baseline-2026-09-12/report-corrected.json /tmp/answers-after/report.json
```

The comparator checks experiment compatibility, complete usage, complete repeats,
and per-task accuracy before flagging a lower-cost point estimate. Retain all new
artifacts and rerun the [mechanical suite](../../graph/README.md) too.

Checks passed: full repository tests, scorer/accounting/rescore tests, production
binary build, live 16-attempt run, self-comparison sanity check, and pre-commit.
See [suite boundaries and instructions](../README.md).
