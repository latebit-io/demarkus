# demarkus-opencode-knowledge

Connect [OpenCode](https://opencode.ai) to an organizational demarkus knowledge system: a shared, versioned markdown catalog behind an HTTPS broker and OAuth.

This plugin complements [`demarkus-opencode-memory`](../opencode-memory/). Memory owns personal memories and working notes; knowledge owns shared, curated, authoritative material. Both reuse `~/.demarkus` registries and one helper binary.

Package installation requires OpenCode V2 with `opencode plugin add` and Git package subdirectory support, plus `bash`, `curl` and `tar` for helper provisioning.

## Features

- `/knowledge-join <broker-url>` validates the broker, records its remote MCP endpoint, registers policy scope, and hands authentication to OpenCode OAuth.
- `/knowledge` navigates joined systems and root hubs.
- `/knowledge-doctor` audits multi-world catalog health without writing.
- `knowledge-promote` curates staged material into a writable destination.
- Session guidance and recall nudges consult shared knowledge first.
- Catalog recall prefers the broker-wide `mark_lookup_all`, one lookup across every readable world, with a per-world fallback for older brokers and plain endpoints.
- Publish and append gates enforce tags, retention confirmation, and style rules; the server enforces the system's write policy.
- Co-installation with the memory plugin uses one gate owner, preventing duplicate blocks and warnings.

No demarkus server or capability token is installed. The plugin bootstraps the small `demarkus-plugin` helper used for registry, guidance, gate and nudge behavior. OpenCode owns broker OAuth credentials and package updates.

## Install

```bash
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-knowledge'
```

No separate installer or npm publication is needed. Existing script installs must [migrate first](../opencode-memory/MIGRATION.md).

For development, configure the checkout directory in `opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/absolute/path/to/demarkus/plugins/opencode-knowledge"]
}
```

Restart OpenCode. Existing systems already present in both `~/.demarkus/knowledge-systems` and `~/.config/mcp/mcp.json` load automatically; use `/knowledge` to verify them. To add a new system, run:

```text
/knowledge-join https://knowledge.example.com
```

Restart once more after joining so OpenCode loads the new remote MCP entry. Open `/mcps`, select the server, and sign in.

## Layout

- Plugin, generated commands, skill and runtime assets: the OpenCode-managed package.
- Joined slugs: `~/.demarkus/knowledge-systems`
- Shared endpoints: `~/.config/mcp/mcp.json`

Explicit MCP entries (`mcp.servers[slug]` in V2, `mcp[slug]` in V1) win over generated entries, including disabled servers and authentication settings. Reserved local-memory slugs are disabled in both versions. Joined systems without a shared endpoint produce a startup diagnostic; rerun `/knowledge-join` in OpenCode to repair them.

V2 registers commands and skills through transforms and adds guidance to model context without rewriting persisted user prompts. Existing command and skill definitions take precedence. Bootstrap notices go to the server log.

## Update

```bash
opencode plugin check
opencode plugin update 'github:latebit-io/demarkus#main::path:plugins/opencode-knowledge'
```

Replace `main` with a full commit SHA to pin a revision. Full commit pins are not updated automatically. The bundled bootstrap retains the shared helper's minimum-version pin and never downgrades a newer helper.

## Remove

```bash
opencode plugin remove 'github:latebit-io/demarkus#main::path:plugins/opencode-knowledge'
```

Use the exact configured target if pinned. Removal leaves shared registrations and OAuth state intact. Explicit MCP configuration remains managed by the user.

## Development

Runtime prompt files under `commands/`, `context/`, and `skills/` are generated from `plugins/prompt-source/`. Edit the canonical templates, then run `cd tools && go run ./plugin-prompts write`; CI rejects manual drift.

`src/package.ts` is a checked copy of the memory package's prompt-registration adapter. Both packages ship their own generated artifacts and have no cross-package runtime dependency. OpenCode remains outside the [brand generator's supported bases](../brands/README.md).

```bash
cd plugins/opencode-knowledge
npm install
npm test
npm run typecheck
shellcheck -S warning scripts/bootstrap.sh
```
