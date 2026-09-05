# Changelog

## 0.5.95

Frontmatter descriptions containing a colon are now quoted so strict YAML parsers load them; unquoted, Cursor (js-yaml) dropped `/knowledge-doctor` and the `knowledge-promote` skill. The generator now rejects frontmatter that is not valid YAML.

## 0.5.83

- Guidance and commands now say "memory" for the personal store (formerly "soul") and point at the renamed `/memory-*` commands.

## 0.5.66

- Prefer broker-wide `mark_lookup_all` for shared catalog recall, with fallback for older brokers and plain endpoints.

## 0.5.55

- Generate session guidance, the promotion skill, and slash-command prompts from the shared `plugins/prompt-source` corpus.
- Adopt the hardened join and bounded doctor workflows across harnesses.
- Load command descriptions from generated frontmatter and substitute arguments exactly once.

## 0.5.10

- Documentation style gate in the shared binary (tools release with the style
  gate required) plus writing standards guidance: session guidance points at
  mark://root/.well-known/demarkus/style.md, and /knowledge-doctor gains a
  style-guide check (em dashes, duplicate headings) over fetched bodies.

## 0.5.9

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

## 0.5.8

- Repin the shared `demarkus-plugin` binary to `tools/v0.8.0`: the version
  retention release: the unified write-time gate now guards
  `metadata.retention` on publish/append at ask severity (pi treats ask as
  block), covering the knowledge surface as well as souls;
  `mark_graph_publish` is exempt by design.

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
