# Section-first reader comparison: 2026-09-12

**Existing retrieval features work substantially better with section-first guidance.**
The change is reader policy, not production retrieval software. The comparator
verified identical server/MCP binary hashes, model, source snapshot, questions,
scoring and non-policy settings. Both reader prompts use the same answer/citation
contract; only retrieval guidance differs.

## Results

Eight actual-soul questions, two fresh sessions each; `openai/gpt-6-astra`, low.
The same 311-document, 2,184-version snapshot was used and remained unchanged.

| Metric | Broad budgeted body lookup | Section-first |
|---|---:|---:|
| Correct cited answers, common rubric v2 | 16/16 | 16/16 |
| All model tokens | 210,381 | 95,041 |
| Model tokens per correct answer | 13,148.81 | 5,940.06 |
| Total tool-result tokens, o200k_base | 67,224 | 12,364 |
| Tool-result tokens per correct answer | 4,201.50 | 772.75 |
| Model turns | 51 | 50 |
| MCP calls | 36 | 36 |
| Internal protocol requests | 178 | 52 |

Model-token cost fell **54.8%**, tool-result tokens **81.6%**, with the same
correct outcomes under the common corrected rubric. These are point estimates
on a small scoped recall cohort, not a statistical significance claim.

The earlier target of about 1,500 tokens concerned tool results. Section-first
averages 773 result tokens here. Its 5,940 total model tokens also count prompts,
schemas, cached input and conversation replay across all model turns. The large
improvement comes from returning and replaying less irrelevant text, not fewer
MCP calls. No server, MCP handler, ranking or expansion implementation changed.

## Policy difference

- `budget-body`: the original benchmark favored budgeted body lookup. It often
  searched a bare ADR number across the whole corpus, requesting 2,500–4,000
  token budgets and expanding unrelated documents before fetching the target.
- `section-first`: lookup without expansion by default, starting at limit 3;
  descriptive terms and appropriate scope; catalog lookup for named documents,
  body mode for section text/misses; targeted section fetches; optional focused
  expansion capped in guidance at 1,500. This is a guidance target, not a changed
  server limit. Actual tool choices remain the model's.

The CLI now defaults to `section-first`. Reproduce the original reader with
`-reader-policy budget-body`. Old reports without a policy field retain their
original budget-body meaning.

## Grading correction applied to both runs

Under the original unchanged rubric, the runs scored 16/16 and 14/16. Both misses
were the OAuth question. Both answers had correct values and every mandatory
citation, plus a valid extra quote from ADR 0011's Consequences section:
“Adding a host is a code change and a release, not an operator setting.”

The scorer had rejected that supplemental support as an unrecognized extra. It
now distinguishes mandatory evidence from explicitly approved supplemental
evidence. The latter must still be present in the cited immutable source and
the reader's observed text, and cannot replace mandatory evidence. Unapproved
extras and fabricated, unread or wrong-version quotes still fail.

A separate `questions-v2/` retains byte-identical tasks and all original expected
answers and required evidence, adding only that approved supplemental quote.
Both original traces were rescored against this same rubric. No new model calls
or selected retries were used. Original rubric and raw reports remain untouched;
each graded-v2 report records its original report digest. The rubric change
affects the accuracy denominator, not the measured token reduction.

## Artifacts and commands

Portable, versionable definitions and metrics-only copies now live in the
[standard corpus package](../corpora/soul-2026-09-12/README.md). Use its verified
restore and benchmark commands from a fresh checkout. Paths below preserve the
original local evidence from this comparison.

Private files under `tools/benchmarks/answers/soul-local/2026-09-12/`:

- `baseline/report.json`, `section-first/report.json`: unchanged raw-run scores.
- `baseline/report-graded-v2.json`, `section-first/report-graded-v2.json`: common grading.
- `questions-v2/`: tasks and revised rubric; original files remain in the parent.
- `policy-comparison-v2.json`: comparison output.

Reproduce this ablation:

```bash
tools/bin/demarkus-answer-bench compare-policies tools/benchmarks/answers/soul-local/2026-09-12/baseline/report-graded-v2.json tools/benchmarks/answers/soul-local/2026-09-12/section-first/report-graded-v2.json
```

After a production software change, use the section-first run as the before
baseline and the **strict** comparator:

```bash
make answer-bench
tools/bin/demarkus-answer-bench -reader-policy section-first -corpus tools/benchmarks/answers/soul-local/2026-09-12/corpus -questions tools/benchmarks/answers/soul-local/2026-09-12/questions-v2 -origin mark://soul.demarkus.io -out /tmp/soul-after
tools/bin/demarkus-answer-bench compare tools/benchmarks/answers/soul-local/2026-09-12/section-first/report-graded-v2.json /tmp/soul-after/report.json
```

`compare-policies` allows only the two recognized prompts/configurations and
requires identical retrieval binaries. It still rejects model, corpus, tasks,
rubric or other-setting changes and incomplete accounting. `compare` continues
to require the same reader policy and prompt. Neither overwrites old results.

[Original real-soul baseline](soul-baseline-2026-09-12.md) · [Runner details](README.md).
