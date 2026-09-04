# Changelog

## 0.13.83

Slash commands renamed from `/memory-*` back to `/soul-*` (`/soul`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-refresh`) and prose now says "soul" for the personal store, so it is not confused with the host's built-in memory feature. No aliases for the old names. Plugin name, MCP server id, and `mark_*` tools unchanged. Prompt templates trimmed for token cost.

## 0.13.76

Initial Cursor port of demarkus-memory. Cursor Plugin format with bash hook shims over the shared `demarkus-plugin` binary: sessionStart guidance and provisioning, a beforeMCPExecution publish tag-gate and destination gate (deny, ask, or allow with a warning), the ADR promote nudge, a sentinel-driven stop journal nudge, generated `/memory-*` commands and the `remember` skill, and `registry mcp ... --harness cursor` for joined memories. The recall nudge is not ported: Cursor's beforeSubmitPrompt hook cannot inject context.
