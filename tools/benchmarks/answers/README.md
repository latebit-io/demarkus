# End-to-end answer and token benchmark

Commit-ready corpus definition, pinned manifest, questions and metrics-only
baselines: [standard soul corpus](../corpora/soul-2026-09-12/README.md).
The compressed archive and raw model traces remain local and git-ignored.

Primary software-comparison baseline: [section-first on real soul](section-first-comparison-2026-09-12.md).
The [original real-soul run](soul-baseline-2026-09-12.md) preserves the broad-body reader.
Earlier [synthetic calibration](baseline-2026-09-12/README.md) remains a separate cohort.

Runs a real model through production demarkus MCP tools and a local production
server. Primary metric:

**All model tokens spent / correct answers with complete source support.**

Incorrect answers stay in the numerator. Zero correct answers yields an undefined
ratio, not zero. Missing usage or an incomplete cohort invalidates comparison.
Correct abstentions count as correct on the absent-answer task and have their own
category; answerable categories still cannot regress unnoticed.

## Run

For the copied real corpus, use the commands in the primary baseline above.
The no-corpus command below uses the embedded synthetic fixture.

From the repo root, with OpenCode **1.18.30** installed and signed in:

```bash
make answer-bench
tools/bin/demarkus-answer-bench -out /tmp/answers-before
# After a roadmap slice, rebuild all three binaries and use a new output directory:
make answer-bench
tools/bin/demarkus-answer-bench -out /tmp/answers-after
tools/bin/demarkus-answer-bench compare /tmp/answers-before/report.json /tmp/answers-after/report.json
```

Defaults: `section-first` reader policy, `openai/gpt-6-astra`, reasoning variant `low`, 8 model iterations,
4096 maximum output tokens per model call, 3-minute question timeout, 2 fresh
sessions per question. Eight tasks produce 16 attempts. `-case q1 -repeats 1`
is a smoke run, not a full baseline. `-model`, `-variant`, `-steps`, `-timeout`,
and `-repeats` must match across compared runs. Port 16319 must be unused;
override with `-port` in both runs if needed. `-temp` selects the temporary parent.

Runs use the configured provider account and can consume its paid usage/quota.
No credential is copied into reports. OpenCode's persistent provider authentication
is reused; benchmark configuration, session database, MCP home/graph cache, and
source store are isolated. Fresh child processes load their temporary config;
the user's running OpenCode session and configuration need no restart or edits.
External plugins, personal instructions, skills, filesystem tools, web tools,
delegation, automatic compaction, title generation and summarization are disabled.

## Frozen tasks and scoring

`tools/internal/answerbench/fixtures/` separates:

- `corpus.json`: ten synthetic documents, including two retention-policy revisions.
- `tasks.json`: natural questions and requested answer field types.
- `rubric.json`: exact factual values and required source evidence, held by scorer.

Cases cover direct retrieval, two-source procedures, supersession, historical
revision selection, differing regional policies, body vocabulary, missing facts,
and section selection. No answer paths, queries, stop conditions, categories, or
expected values are passed to the reader. The model chooses searches and tool
calls. A read-only proxy exposes production schemas and handlers and confines
URL arguments to the fixture endpoint. No live soul/world content is loaded.

Answers use typed JSON fields (numbers, exact source identifiers, ordered step
identifiers), avoiding an LLM judge or a permissive keyword match. Each field must
have complete supporting citations. A citation needs an immutable `/doc.md/vN#section`
URL and an exact supporting sentence both in that source and in text returned to
the reader. Wrong versions, fabricated quotes, irrelevant citations, snippets
without expanded evidence, extra fields and missing sources fail scoring.
Whitespace is normalized; answer values and array order remain exact.

Scoring version `section-provenance-v2` keeps observations scoped to their
document/version/anchor. Equal text in sibling sections cannot satisfy each
other's citations. Full-document fetches and explicitly returned nested sections
remain usable; a parent citation must contain the required section and identify
its passage unambiguously. Storage errors abort scoring rather than count as
wrong answers. Reports with different scoring versions are not comparable.

The two-source procedure task requires both step-definition quotes and the
dependency handoff quote linking the procedure to its audit requirement.

Budgeted expansions currently omit revisions. Their observed revision is resolved
against this read-only frozen corpus; ordinary fetch observations use the returned
version header. This is not a claim that live context expansion pins revisions.

This small synthetic suite is an initial end-to-end baseline. It is **not** a
held-out real-world evaluation or evidence of broad semantic reasoning quality.
Before broad relevance claims, add independently authored, unseen question cohorts
over frozen real corpora. Preserve this cohort and report new cohorts separately.

Rubrics may list explicitly approved `supplemental` evidence per field. It must
be source-valid and observed, but never replaces mandatory `evidence`. This
avoids rejecting correct answers for additional supporting citations without
accepting arbitrary extras. Grade both comparison arms with the same rubric.

## Token accounting

Usage comes from every completed OpenCode model-step event, not a byte estimate:

`total = uncached input + cache read + cache write + visible output + reasoning`

OpenCode 1.18.30 already subtracts cached tokens from input and reasoning from
output. Its optional `total` field is checked against the disjoint sum, never
added again. Repeated events are deduplicated by part ID. Input includes all
provider-counted system prompts, tool schemas, tool arguments/results, and replayed
conversation context on each turn. Cache tokens still count as computational
context; pricing discounts do not turn them into zero tokens. This is a token
metric, not a dollar invoice; OpenCode can report $0 for subscription access.

Failed answers with complete usage remain charged. Interrupted/error traces keep
their known spend but invalidate the total ratio. Provider requests that fail
before emitting usage may have unknown billable spend; no invoice-level accuracy
is claimed for such requests. Hidden auxiliary model tasks are disabled.

## Artifacts and interpretation

- `report.json`: model/settings/fixture/prompt/config fingerprints, binary hashes,
  per-attempt usage buckets, model turns, tool calls, internal protocol requests,
  wall time, final answers, reasons for misses, overall/category summaries.
- `qN-R-events.jsonl`: raw model-step usage and tool-output evidence.
- `qN-R-prompt.txt`, `qN-R-stderr.txt`: exact question input and diagnostics.
- `server.jsonl`: protocol request log; per-question counts include MCP startup
  discovery traffic, but exclude the runner's initial readiness probe.

Output directories cannot be reused. A report is comparable only when all scheduled
attempts finished and `summary.usage_complete` is true. Failed runs still write
partial reports. Keep the original baseline immutable. The comparator rejects
changed fixtures, models, prompts, config, settings, missing repeats, and unknown
usage. It flags per-task accuracy regressions even when the aggregate ratio falls.
Its improvement flag is a **point estimate**, not statistical significance.

If calibration exposes a rubric error, preserve the original report and use
`demarkus-answer-bench rescore ORIGINAL.json NEW.json`. This reuses identical
reader traces and usage, records the original report digest, and changes the
rubric fingerprint. It rejects different source/task snapshots and existing
output files. A grading correction is not an optimization gain; the comparator
rejects original-vs-corrected rubrics. Freeze the corrected rubric before fixes.

For copied stores, `rescore` also accepts `-corpus ROOT -questions DIR` before
the original and new report paths. The copied corpus fingerprint must match.

`export-metrics PRIVATE.json NEW.json` produces a versionable report without
retrieved text, answers, arguments, session IDs or diagnostics. Strict and policy
comparisons preserve its token/call counts; regrading requires the private trace.

Model routing, provider caches and serving revisions can vary despite a fixed
model ID. Repeat paired before/after runs on the same machine; two attempts per
task establish a starting observation, not a reliable uncertainty bound. Changes
to tools or retrieval implementations are identified by their binary hashes.

Keep the [mechanical graph baseline](../graph/README.md) alongside this suite.
Delivery and evaluation live in the [roadmap](mark://soul.demarkus.io/roadmap/graph-focus.md)
and [evaluation design](mark://soul.demarkus.io/plans/graph-focus/evaluation.md).
