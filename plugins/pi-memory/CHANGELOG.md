# Changelog

## 0.12.9

- Repin the shared `demarkus-plugin` binary to `tools/v0.8.1`, which
  provisions `demarkus-server` at `server/v0.20.0` and `demarkus-mcp` at
  `client/v0.18.0` — the version retention core: a `retention: N` publish
  metadata key prunes a document to its newest N versions
  (`mark_graph_publish` sets it by default on the generated graph document).
- Session guidance: never set `metadata.retention` unless the user explicitly
  asked for it — a positive-integer retention permanently deletes all but the
  newest N versions on that write and every later write carrying the key. The
  shared gate binary (tools/v0.8.0+) asks for confirmation when a write
  carries a prunable retention value (`DEMARKUS_RETENTION_STRICTNESS`
  relaxes or hardens it).

## 0.12.8

- Repin the shared `demarkus-plugin` binary to `tools/v0.8.0` — the version
  retention release: the unified write-time gate now guards
  `metadata.retention` on publish/append at ask severity (pi treats ask as
  block), covering soul and knowledge surfaces alike; `mark_graph_publish`
  is exempt by design.

## 0.12.7

- Repin the shared `demarkus-plugin` binary to `tools/v0.6.1`, which provisions
  `demarkus-mcp` at `client/v0.17.0` — the MCP resources + prompts release:
  documents attach as MCP resources (mark:// URI template, `#anchor` section
  attach, picker entries populated from the soul's top level), and orient /
  recall / whats-new ship as server-vended prompt commands.

## 0.12.5

- Repin the shared `demarkus-plugin` binary to `tools/v0.4.1`, which provisions
  `demarkus-mcp` at `client/v0.15.0` — the MCP client-ergonomics release:
  size-adaptive `mark_fetch` (outline mode for large documents, `url#anchor`
  section slicing, `force` override), per-session unchanged-fetch dedup, and
  the new `mark_explore` neighborhood-card tool.

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
