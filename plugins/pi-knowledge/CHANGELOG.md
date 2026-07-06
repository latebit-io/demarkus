# Changelog

## 0.5.7

- Repin the shared `demarkus-plugin` binary to `tools/v0.6.1` (provisions
  `demarkus-mcp` at `client/v0.17.0` with MCP resources + prompts). Keeps
  this plugin's pin in lockstep with the memory plugins, since all four
  share the same `demarkus-plugin` binary install.

## 0.5.5

- Repin the shared `demarkus-plugin` binary to `tools/v0.4.1` (provisions
  `demarkus-mcp` at `client/v0.15.0` with the MCP client-ergonomics work:
  size-adaptive fetch, `#section` slicing, session dedup, `mark_explore`).
  Keeps this plugin's pin in lockstep with the memory plugins, since all four
  share the same `demarkus-plugin` binary install.

## 0.5.4

- Repin the shared `demarkus-plugin` binary to `tools/v0.3.5` (provisions
  `demarkus-mcp` at `client/v0.13.2`, which fixes the QUIC connection-pool
  wedge). Keeps this plugin's pin in lockstep with the memory plugins, since
  all four share the same `demarkus-plugin` binary install.

## 0.5.3

- Fix: slash commands (`/knowledge`, `/knowledge-join`, `/knowledge-doctor`)
  registered but never executed. The command handler injected the skill body with
  `pi.sendMessage(...)` and no options; on an idle session pi appends the message
  to history without starting a turn, so the agent never acted on it. Pass
  `{ triggerTurn: true }` so the command starts a turn.
