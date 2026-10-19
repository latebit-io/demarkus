# CLAUDE.md

## How I work

- No sycophancy — on code or ideas. Critically review before presenting: layering
  violations, missing edge cases, state desync, stale references, channel blocking,
  rune vs byte vs cell-width confusion, silent error paths, leaky abstractions,
  wrong architectural layer, unbounded long-lived state, substrings or subslices
  that pin a larger buffer.
- Memory tracks live data, not history. Long-lived state (globals, caches,
  indexes, snapshots) dies with its owner or evicts; a cap is not eviction.
  Clone what you keep from a larger buffer. New long-lived state ships with a
  `memtest.Retained` test on production-shaped data.
- Challenge ideas before agreeing: what's the downside? what breaks? what's the
  simpler alternative? is this the right problem? Push back when something doesn't
  hold up — disagreement backed by reasoning is expected.
- Never silently swallow errors. Every error is explicitly handled — logged or
  surfaced. No `_ = fn()` without a comment.
- Code comments are terse: 1-3 lines stating only what the code can't show
  (why, invariants, platform caveats). No narration, no essays. When touching
  code that has verbose comments, tighten them in the same edit.
- After each implementation, run tests and `bash pre-commit.sh`.

## Benchmarks

Standalone benchmark runners, evaluation datasets, performance probes, and reports
belong on the `benchmarks` branch, never in changes destined for `main`. Keep
ordinary correctness and regression tests with production code. Preserve raw
evidence under ignored benchmark artifacts.

## Prompt and tool-description token budget

Everything under `plugins/prompt-source/` and every MCP tool description
(`client/cmd/demarkus-mcp`, `knowledge/internal/broker/gateway/mcp_tools_list.go`) is
paid in tokens on every session, invocation, or request. Write terse:
fragments, no filler, no rationale the step implies. Keep behavior intact,
nothing volatile, no em dashes. See `plugins/prompt-source/README.md`.

## Prompt procedures are edited whole

A skill or command template has no compiler. Never fix a review finding by
changing one sentence: read the whole step and the steps it hands off to,
rewrite the step, define every named state (`incomplete-dedup`,
`catalog-only`) once as terminal or carried forward and grep for every other
use, give every call in the step its failure line, then reread against the
skill's top-level rule. Repeated findings on one paragraph mean it was
patched, not rewritten.
