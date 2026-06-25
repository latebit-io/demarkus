# Changelog

## 0.12.4

- Repin the shared `demarkus-plugin` binary to `tools/v0.3.5`, which provisions
  `demarkus-mcp` at `client/v0.13.2`. That release fixes a connection-pool bug
  where a timed-out QUIC stream-open on a dead pooled connection was never
  evicted/redialed, wedging every subsequent fetch to a soul until the MCP
  server restarted.

## 0.12.3

- Fix: slash commands (`/soul`, `/soul-context`, `/soul-journal`, `/soul-init`,
  `/soul-join`, `/soul-default`, `/soul-status`, `/soul-doctor`, `/soul-refresh`,
  `/promote`, `/promote-scan`) registered but never executed. The command handler
  injected the skill body with `pi.sendMessage(...)` and no options; on an idle
  session pi appends the message to history without starting a turn, so the agent
  never acted on it. Pass `{ triggerTurn: true }` so the command starts a turn.
