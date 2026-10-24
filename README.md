<!-- markdownlint-disable-next-line MD041 -->
> [!WARNING]
> **Beta.** Demarkus is pre-1.0 and under active development. Features, components, binaries, plugins and commands are in flux: any of them can change, move or be removed between releases. Pin a release for anything you depend on.

# Demarkus

**Memory for agents, knowledge for teams**

Your agent records decisions, lessons, and progress as it works, and recalls them in every later session. When a note is ready for others, it moves up to your team's shared knowledge base. Every note is versioned markdown, and every link between notes is an edge in one knowledge graph.

- **[Memory](https://www.demarkus.io/memory/)**: your agent's own versioned notes, filed as it works and recalled before the next task. Local, self-hosted, or hosted.
- **[Knowledge base](https://www.demarkus.io/knowledge/)**: one shared catalog behind your login. Notes arrive from memory through a review gate; policy blocks secrets and duplicates on every write.
- **[Graph](https://www.demarkus.io/graph/)**: links and typed relations form a persistent graph. Agents follow edges instead of loading files; people browse it in the [library](https://www.demarkus.io/library/).

![Client install demo](docs/images/demarkus-client.gif)

## Start here

Give your agent memory. The plugin brings its own binaries: nothing to configure.

**Claude Code**

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

**Claude Cowork**: add this repository as a marketplace (Customize, Plugins) and install `demarkus-memory`. What differs in Cowork: [Claude Cowork](plugins/claude-code/README.md#claude-cowork).

**Cursor**

```text
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
```

**OpenCode**

```bash
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
```

**pi**

```bash
pi install npm:pi-mcp-adapter
pi install git:github.com/latebit-io/demarkus-pi-memory
```

All plugins share `~/.demarkus`: one memory across every agent on the machine. Details per host: [Claude Code](plugins/claude-code/README.md), [Cursor](plugins/cursor-memory/), [OpenCode](plugins/opencode-memory/), [pi](plugins/pi-memory/).

## Join a knowledge base

Install the knowledge plugin beside the memory plugin, then join your system and sign in through the browser:

```text
/plugin install demarkus-knowledge@demarkus
/knowledge-join https://knowledge.example.com
```

No server to run, no token to copy. Same plugin for [Cursor](plugins/cursor-knowledge/), [OpenCode](plugins/opencode-knowledge/), and [pi](plugins/pi-knowledge/). Try the live system: `/knowledge-join https://knowledge.demarkus.io`.

## How it works

```text
  agent ──record──▶ MEMORY ──/promote──▶ KNOWLEDGE BASE ──▶ LIBRARY
    ▲               private   review     shared, governed    people
    └──────────── recall · consult · follow the graph ◀───────┘
```

- **Record as you go.** The agent files decisions, lessons, patterns, and a daily journal where they belong. Hooks catch untagged writes and missing journal entries.
- **Recall with evidence.** `mark_lookup` returns the matching section and its related documents, not whole files.
- **Promote what matters.** `/promote` distills a note, strips personal framing and secrets, checks for duplicates, and routes it to a writable world. You approve before it lands.
- **Keep every version.** Nothing is overwritten. Every change has an author and a history.

The project keeps its own memory at `mark://soul.demarkus.io`; read it in the browser at [soul.demarkus.io](https://soul.demarkus.io).

## Run a knowledge system

`demarkus-knowledge` is one process: many worlds (one GCS bucket each, selected by TLS SNI), OIDC single sign-on, the MCP gateways, and the cross-world graph. It runs as two or more stateless replicas; writes commit by compare-and-swap on the bucket, so there is no leader and no persistent volume.

```bash
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --create-namespace --values knowledge-system.yaml
```

The [`demarkus-knowledge-system`](deploy/helm/demarkus-knowledge-system/) umbrella declares every world once in one values file and can add the [library](https://github.com/latebit-io/demarkus-library). Given a second hostname, the same process serves **hosted memory**: one private world per signed-in person, for Claude Desktop, ChatGPT, Cursor, or any MCP host. See [Kubernetes & Helm](docs/site/deployment/kubernetes.md) and [Memory gateway](docs/site/tools/memory-broker.md).

## Self-host by hand

```bash
# macOS / Linux: server + client
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash

# Client only (CLI, TUI, MCP)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only

# Read-only chrooted server (Linux)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-readonly.sh | sudo bash -s -- --domain yourdomain.com
```

```bash
demarkus mark://soul.demarkus.io/index.md       # fetch
demarkus-tui mark://soul.demarkus.io/index.md   # browse, with a graph view
demarkus-server -root ./docs/site               # serve a directory
```

Platform guides: [www.demarkus.io/install](https://www.demarkus.io/install/).

## Binaries

| Binary | Purpose |
|--------|---------|
| `demarkus-server` | One world: your local or self-hosted soul |
| `demarkus-knowledge` | Many worlds, OIDC single sign-on, MCP gateways for knowledge and hosted memory, the cross-world graph; with no broker config it serves the worlds alone |
| `demarkus` | CLI: fetch, publish, edit, lookup, graph, watch, okf |
| `demarkus-tui` | Terminal browser with a persistent graph view |
| `demarkus-mcp` | MCP server over one world: the protocol verbs as tools, plus graph, backlinks, doctor, curate and promote |
| `demarkus-plugin` | Shared core of the agent plugins; installed by them |
| `demarkus-token` | Capability tokens and join URLs |
| `demarkus-publish` | Write straight to the store for read-only installs |

## Built on the Mark Protocol

Versioned markdown over QUIC (UDP 6309), addressed as `mark://<world>/<path>`. Every write is an immutable, hash-chained version. Capability tokens gate writes; reads are open unless a world enables read auth. No tracking, no cookies, nothing executes.

**Verbs**: `FETCH` · `LIST` · `VERSIONS` · `LOOKUP` · `WATCH` · `PUBLISH` · `APPEND` · `ARCHIVE`

A document is content-compatible with [Google's Open Knowledge Format v0.1](https://github.com/GoogleCloudPlatform/knowledge-catalog); `demarkus okf` validates, imports, and exports bundles. See the [specification](docs/SPEC.md) and [design rationale](docs/DESIGN.md).

## Build from source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all   # protocol, server, client, tools, knowledge
```

Requires Go 1.26+. Binaries land in `server/bin/`, `client/bin/`, `tools/bin/`, and `knowledge/bin/`.

## Documentation

- [Website](https://www.demarkus.io/): memory, knowledge, graph, install guides
- [Docs](docs/index.md): operator and contributor reference
- [Glossary](docs/site/reference/glossary.md): one name per concept
- [Ecosystem](https://www.demarkus.io/ecosystem/): plugins, readers, and tools

## License

- Implementation: AGPL-3.0-only ([LICENSE](LICENSE))
- Agent plugins: MIT ([plugins/LICENSE](plugins/LICENSE))
- Protocol specification: CC0-1.0 ([LICENSE-PROTOCOL](LICENSE-PROTOCOL))
