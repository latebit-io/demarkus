# Changelog

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
