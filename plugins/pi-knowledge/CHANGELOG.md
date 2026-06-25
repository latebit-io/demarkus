# Changelog

## 0.5.3

- Fix: slash commands (`/knowledge`, `/knowledge-join`, `/knowledge-doctor`)
  registered but never executed. The command handler injected the skill body with
  `pi.sendMessage(...)` and no options; on an idle session pi appends the message
  to history without starting a turn, so the agent never acted on it. Pass
  `{ triggerTurn: true }` so the command starts a turn.
