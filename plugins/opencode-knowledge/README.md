# demarkus-opencode-knowledge

Connect [OpenCode](https://opencode.ai) to an organizational demarkus knowledge system: a shared, versioned markdown catalog behind an HTTPS broker and OAuth.

This plugin complements [`demarkus-opencode-memory`](../opencode-memory/). Memory owns personal souls and working notes; knowledge owns shared, curated, authoritative material. Both reuse `~/.demarkus` registries and one helper binary.

## Features

- `/knowledge-join <broker-url>` validates the broker, records its remote MCP endpoint, registers policy scope, and hands authentication to OpenCode OAuth.
- `/knowledge` navigates joined systems and root hubs.
- `/knowledge-doctor` audits multi-world catalog health without writing.
- `knowledge-promote` curates staged material into a writable destination.
- Session guidance and recall nudges consult shared knowledge first.
- Publish and append gates enforce mirrored system policy, retention confirmation, and style rules.
- Co-installation with the memory plugin uses one gate owner, preventing duplicate blocks and warnings.

No demarkus server or capability token is installed. The plugin bootstraps the small `demarkus-plugin` helper used for registry, guidance, gate, nudge, and update behavior. OpenCode owns broker OAuth credentials.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash
```

Or from a checkout:

```bash
demarkus/plugins/opencode-knowledge/install.sh
```

Restart OpenCode. Existing systems already present in both `~/.demarkus/knowledge-systems` and `~/.config/mcp/mcp.json` load automatically; use `/knowledge` to verify them. To add a new system, run:

```text
/knowledge-join https://knowledge.example.com
```

Restart once more after joining so OpenCode loads the new remote MCP entry. Complete OAuth when prompted, or run `opencode mcp auth <slug>`.

## Layout

- Plugin: `$XDG_CONFIG_HOME/opencode/plugins/demarkus-knowledge.ts`
- Skill: `$XDG_CONFIG_HOME/opencode/skills/knowledge-promote/SKILL.md`
- Runtime assets: `~/.demarkus/opencode-knowledge/`
- Joined slugs: `~/.demarkus/knowledge-systems`
- Shared endpoints: `~/.config/mcp/mcp.json`
- Mirrored policy: `~/.demarkus/plugin-knowledge.*.<slug>`

Explicit `config.mcp[slug]` entries win over generated entries. Joined systems without a shared endpoint remain disabled and produce a startup diagnostic; rerun `/knowledge-join` in OpenCode to repair them.

## Update

Rerun the installer and restart OpenCode. `DEMARKUS_REF=<branch|tag|sha>` pins standalone installation to a revision. The helper checks `main` once per day and shows a notify-only update toast; disable it with `DEMARKUS_UPDATE_CHECK=0` or `~/.demarkus/plugin.update-check` containing `0`.

## Remove

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash -s -- --uninstall
```

Uninstall leaves shared registrations and OAuth state intact. Before removing a joined system, confirm that deleting its shared endpoint will also disconnect pi. Then run `opencode mcp logout <slug>`, `registry mcp remove <slug>`, and `registry knowledge-unregister <slug>`, and restart OpenCode. A manually configured `config.mcp[slug]` entry must be removed through the same config mechanism that created it.

## Development

```bash
cd plugins/opencode-knowledge
npm install
npm test
npm run typecheck
shellcheck -S warning install.sh scripts/bootstrap.sh
```
