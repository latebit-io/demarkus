# Changelog

## 0.13.88

Slash commands renamed from `/memory-*` back to `/soul-*` (`/soul`, `/soul-init`, `/soul-join`, `/soul-default`, `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-refresh`) and prose now says "soul" for the personal store, so it is not confused with the host's built-in memory feature. No aliases for the old names. Plugin name, MCP server id, and `mark_*` tools unchanged. Prompt templates trimmed for token cost.

## 0.13.76

- Remove the deprecated `/soul-*` command aliases; use the `/memory-*` names. The pinned binary is unchanged and still accepts its `soul-*` helper names.

## 0.13.75

- Call the canonical `memory-*` helper names now that the pinned binary (tools 0.27.0) provides them: `registry memory-join`, `registry memory-default`, `mcp-serve --memory`, and the `memoryWrite` session-end signal. The `soul-*` names stay accepted by the binary for one more release; MCP entries registered earlier with `--soul` keep working.

## 0.13.72

- Rename the personal store from "soul" to "memory" across guidance, commands, and the skill: `/soul-*` commands become `/memory-*`; the `soul-memory` skill becomes `memory`. The old command names keep working as deprecated aliases for one release. On-disk state under `~/.demarkus` is unchanged.

## 0.13.41

- Generate session guidance, the memory skill, and all slash-command prompts from the shared `plugins/prompt-source` corpus.
- Load command descriptions from generated frontmatter and substitute arguments exactly once.
- Reject stale generated prompts and foreign-harness terms in CI.

## 0.13.27

- Update check on session start: `demarkus-plugin update-check` compares the shipped `package.json` version against the manifest on the monorepo's `main`, and the extension notifies when a newer release exists (notify-only, never self-installing). Throttled to once per 24h by the binary, silent when offline, turned off with `DEMARKUS_UPDATE_CHECK=0` or `~/.demarkus/plugin.update-check`. Requires a tools release carrying `update-check`; until the pin moves, the check is a no-op.

## 0.13.0

- `/soul-join` accepts a join URL (`mark://host#token=...`, as emitted by
  install.sh and `demarkus-token join`): the fragment supplies the capability
  token, so joining a remote soul is one paste-able string instead of
  host + token assembly (shared binary; requires the tools release with
  join-URL support).

## 0.12.10

- Documentation style gate in the shared binary (tools release with the style
  gate required): publishes are checked for a frontmatter fence opening the
  body, a missing H1, em dashes, and duplicate headings (headings are section
  anchors). Default severity warn; DEMARKUS_STYLE_STRICTNESS adjusts. Session
  guidance explains the gate.

## 0.12.9

- Repin the shared `demarkus-plugin` binary to `tools/v0.8.1`, which
  provisions `demarkus-server` at `server/v0.20.0` and `demarkus-mcp` at
  `client/v0.18.0`: the version retention core: a `retention: N` publish
  metadata key prunes a document to its newest N versions
  (`mark_graph_publish` sets it by default on the generated graph document).
- Session guidance: never set `metadata.retention` unless the user explicitly
  asked for it; a positive-integer retention permanently deletes all but the
  newest N versions on that write and every later write carrying the key. The
  shared gate binary (tools/v0.8.0+) asks for confirmation when a write
  carries a prunable retention value (`DEMARKUS_RETENTION_STRICTNESS`
  relaxes or hardens it).

## 0.12.8

- Repin the shared `demarkus-plugin` binary to `tools/v0.8.0`: the version
  retention release: the unified write-time gate now guards
  `metadata.retention` on publish/append at ask severity (pi treats ask as
  block), covering soul and knowledge surfaces alike; `mark_graph_publish`
  is exempt by design.

## 0.12.7

- Repin the shared `demarkus-plugin` binary to `tools/v0.6.1`, which provisions
  `demarkus-mcp` at `client/v0.17.0`: the MCP resources + prompts release:
  documents attach as MCP resources (mark:// URI template, `#anchor` section
  attach, picker entries populated from the soul's top level), and orient /
  recall / whats-new ship as server-vended prompt commands.

## 0.12.5

- Repin the shared `demarkus-plugin` binary to `tools/v0.4.1`, which provisions
  `demarkus-mcp` at `client/v0.15.0`: the MCP client-ergonomics release:
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
