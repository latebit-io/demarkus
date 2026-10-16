---
layout: default
title: Ecosystem
permalink: /ecosystem/
---

# Ecosystem

Software that implements or integrates with the Mark Protocol. First-party tooling lives in the main [demarkus repository](https://github.com/latebit-io/demarkus); third-party implementations are listed as they appear.

## Browsers and readers

### Demarkus Library

The web reading room: server-rendered Go + htmx over one world or a whole knowledge system, with the AI librarian. Trail canvas, hover preview cards, the universe floor, and a cataloging desk with version-guarded writes.

- Repo: [latebit-io/demarkus-library](https://github.com/latebit-io/demarkus-library)
- Guide: [The library](/library/)
- Available through the [knowledge-system chart](/install/stack/) or `install.sh --with-library` on Linux

### Caztor

Cross-platform Java GUI browser for Gemini, Spartan, Gopher, Nex, and **Demarkus**. By Kevin Boone.

- Project page: [kevinboone/caztor](https://github.com/kevinboone/caztor)
- Preliminary, view-only Demarkus support (FETCH latest version)
- Runs on Linux, Windows, and macOS with Java 11+

### Demarkus TUI

The reference terminal browser, built on Bubble Tea and Glamour.

- Source: [client/cmd/demarkus-tui](https://github.com/latebit-io/demarkus/tree/main/client/cmd/demarkus-tui)
- `demarkus-tui mark://your.server/index.md`: full FETCH / LIST / VERSIONS, link-following, graph view, mouse wheel, external URL launching (the URL is a positional argument; there is no `-host` flag)

## Plugins

### Claude Code

Two plugins ship from the same marketplace (`/plugin marketplace add latebit-io/demarkus`). They compose in one install and partition by server scope.

**`demarkus-memory`** is personal agent memory (the [memory pattern](/scenarios/agent-memory/)). Zero-config: spawns a local `demarkus-server`, auto-generates a token, and wires the MCP tools on first session.

- Source: [plugins/claude-code](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code)
- Install: `/plugin install demarkus-memory@demarkus`
- Local memory: `/soul`, `/soul-context`, `/soul-journal`, `/soul-init`, `/soul-status`, `/soul-doctor`, `/soul-curate`, `/soul-archive`
- Remote memories: `/soul-join <join-url>` adds a remote memory and binds it to the project; `/soul-default` picks which one this project writes to
- Promotion: `/promote <path>` lifts a ready note through the curation gate, `/promote-scan` finds candidates, `/soul-refresh` reconciles promoted notes with the shared copy
- Skill: `remember`, which triggers on "remember / recall / save / note" intents

**`demarkus-knowledge`** joins an organizational [knowledge system](/scenarios/knowledge-system/) through MCP OAuth. It runs a shared helper for guidance and gates, with no local world server.

- Source: [plugins/claude-code-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/claude-code-knowledge)
- Install: `/plugin install demarkus-knowledge@demarkus`
- Slash commands: `/knowledge-join <broker-url>`, `/knowledge`, `/knowledge-doctor` (catalog hygiene audit)
- Skill: `knowledge-promote`, the curation cascade that distills, dedups, tags, routes, and gates a document before it lands in the shared catalog
- Recall: session guidance starts shared lookups with `mark_lookup_all`, the broker's one-call catalog search across every readable world

### OpenCode

The OpenCode ports provide the same memory and knowledge-system workflows and
share `~/.demarkus` state with the Claude Code and pi plugins.

**`demarkus-opencode-memory`** installs local memory support:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

**`demarkus-opencode-knowledge`** joins an organizational knowledge system:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash
```

- Source: [plugins/opencode-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-memory) · [plugins/opencode-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/opencode-knowledge)
- Commands: the same `/soul*`, `/promote*`, and `/knowledge*` command sets described above
- Restart OpenCode after installation. The first session provisions the memory; `/soul-status` checks the connection

### pi

The same two plugins, ported to the [pi](https://pi.dev) coding agent and mapped onto its extension API. They share `~/.demarkus` state with the Claude Code and OpenCode plugins (one memory, one token, one set of registries), so all three agents can run on the same machine.

Both need the MCP adapter first:

```bash
pi install npm:pi-mcp-adapter
```

**`demarkus-pi-memory`** is the local memory, with the same provisioning, gates, `/soul*` and `/promote*` commands, plus the `remember` skill.

**`demarkus-pi-knowledge`** joins a knowledge system over the adapter's OAuth, with no tokens stored locally. Commands: `/knowledge`, `/knowledge-join`, `/knowledge-doctor`, plus the `knowledge-promote` skill.

- Source: [plugins/pi-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-memory) · [plugins/pi-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/pi-knowledge)
- Install from the standalone mirrors:

```bash
pi install git:github.com/latebit-io/demarkus-pi-memory
pi install git:github.com/latebit-io/demarkus-pi-knowledge
```

### Cursor

The same two plugins as [Cursor Plugins](https://cursor.com/docs/plugins): bash
hook shims over the shared `demarkus-plugin` binary, prompts generated from the
same source as the other harnesses, and `~/.demarkus` state shared with them.
Both ship from one marketplace manifest at the repository root
(`.cursor-plugin/marketplace.json`).

**`demarkus-memory`** is the local memory, with the same zero-config
provisioning, gates, and nudges; the publish gate runs on `beforeMCPExecution`,
where Cursor supports `ask` natively.

**`demarkus-knowledge`** joins a knowledge system over Cursor's MCP OAuth, with
no tokens stored locally and no local server.

- Source: [plugins/cursor-memory](https://github.com/latebit-io/demarkus/tree/main/plugins/cursor-memory) · [plugins/cursor-knowledge](https://github.com/latebit-io/demarkus/tree/main/plugins/cursor-knowledge)
- Install: add the repository as a marketplace from the plugins panel, then add the plugins:

```
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
demarkus-knowledge → Add
```

- Or symlink a checkout: `ln -s "$PWD/demarkus/plugins/cursor-memory" ~/.cursor/plugins/local/demarkus-memory` (same for `cursor-knowledge`)
- Commands: the same `/soul*`, `/promote*`, and `/knowledge*` command sets described above; the MCP server also provides `orient`, `recall`, and `whats-new` prompts
- Restart Cursor after install; if the `demarkus-memory` MCP server shows as failed before the first conversation provisions the binary, toggle it once under Settings → MCP. Cloud agents run no hooks and cannot reach a local server; the plugins are for the desktop IDE

## Knowledge server

`demarkus-knowledge` hosts GCS-backed worlds, OIDC login, and the knowledge and
memory MCP gateways in one process. The knowledge gateway exposes shared worlds;
the memory gateway gives each identity a private world.

It derives the cross-world graph in process and publishes `/graph/worlds/` and
`/graph.md` to the hub. No separate crawler or indexing-agent deployment is needed.

## CLI and server tools

| Tool | Purpose |
|------|---------|
| `demarkus-server` | Serve a directory of markdown files |
| `demarkus-knowledge` | Many GCS-backed worlds, OIDC gateways, and cross-world graph |
| `demarkus-knowledge-server` | Multi-world QUIC server without the OIDC gateways |
| `demarkus` | CLI: fetch, list, publish, edit, graph, lookup, okf |
| `demarkus-tui` | Terminal browser with graph view |
| `demarkus-mcp` | MCP server for LLM agents: tools, resources, prompts |
| `demarkus-library` | Web reading room with the AI librarian |
| `demarkus-token` | Generate capability-based auth tokens |
| `demarkus-publish` | Write directly to the store for read-only server installs |

## Building your own

The protocol surface is small: QUIC with ALPN `mark`, a text request verb (FETCH, LIST, VERSIONS, LOOKUP, PUBLISH, APPEND, ARCHIVE, WATCH), and YAML-style `---`-delimited metadata. WATCH streams change hints; clients fetch the versions they need. See the [protocol reference](/reference/) for the full spec.

If you ship something that speaks Demarkus, [open an issue](https://github.com/latebit-io/demarkus/issues) or drop a link and we will list it here.
