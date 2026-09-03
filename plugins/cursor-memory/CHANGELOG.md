# Changelog

## 0.13.76

Initial Cursor port of demarkus-memory. Cursor Plugin format with bash hook shims over the shared `demarkus-plugin` binary: sessionStart guidance and provisioning, a beforeMCPExecution publish tag-gate and destination gate (deny, ask, or allow with a warning), the ADR promote nudge, a sentinel-driven stop journal nudge, generated `/memory-*` commands and the `remember` skill, and `registry mcp ... --harness cursor` for joined memories. The recall nudge is not ported: Cursor's beforeSubmitPrompt hook cannot inject context.
