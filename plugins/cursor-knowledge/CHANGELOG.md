# Changelog

## 0.5.95

Initial Cursor port of demarkus-knowledge. Cursor Plugin format with bash hook shims over the shared `demarkus-plugin` binary: sessionStart knowledge guidance, a beforeMCPExecution publish gate (tags, policy axes and fields; deny, ask, or allow with a warning) scoped to joined systems, generated `/knowledge`, `/knowledge-join`, `/knowledge-doctor` commands and the `knowledge-promote` skill, and `registry mcp --harness cursor add-http` for the broker's OAuth MCP entry in `~/.cursor/mcp.json`. The recall nudge is not ported: Cursor's beforeSubmitPrompt hook cannot inject context.
