# Use the Clients

This section covers the three Demarkus client tools: the CLI (`demarkus`), the TUI browser (`demarkus-tui`), and the MCP server (`demarkus-mcp`). Each tool is built on the same Mark Protocol client layer and can be used together.

## Overview

- **CLI** (`demarkus`): scripting, automation, and publishing
- **TUI** (`demarkus-tui`): interactive terminal browser with link navigation
- **MCP** (`demarkus-mcp`): exposes Mark Protocol as tools for LLM agents

If you're new, start with the CLI and confirm you can fetch a document.

## CLI (`demarkus`)

The CLI supports the read/write verbs (`FETCH`, `LIST`, `VERSIONS`, `PUBLISH`, `APPEND`, `ARCHIVE`) plus `edit`, `graph`, `lookup`, and `okf` subcommands.

### Common commands

```bash
# Fetch a document
demarkus --insecure mark://localhost:6309/index.md

# List a directory
demarkus --insecure -X LIST mark://localhost:6309/
demarkus --insecure -X LIST -page-size 250 -cursor '<next-cursor>' mark://localhost:6309/

# Publish a document
demarkus --insecure -X PUBLISH -auth $TOKEN -body "# Hello" mark://localhost:6309/hello.md

# View version history
demarkus --insecure -X VERSIONS mark://localhost:6309/hello.md

# Fetch a specific version
demarkus --insecure mark://localhost:6309/hello.md/v1
```

### Edit a document

Opens a document in `$EDITOR` (falls back to `vi`), then publishes changes when you exit the editor. If the document doesn't exist, creates a new one. Empty documents are rejected.

```bash
# Edit an existing document
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/hello.md

# Create a new document (opens empty editor)
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/new-doc.md
```

### Graph crawl

```bash
demarkus graph --insecure -depth 3 mark://localhost:6309/index.md
```

Graph results are persisted to `~/.mark/graph.json` and accumulate across sessions. Each crawl merges new nodes and edges into the existing graph, so your map of the `mark://` network grows over time. The MCP graph tools seed the store from the connected world's verified `/graph/manifest.md` snapshot, falling back to `/graph.md` for older publishers, so backlinks answer before your first crawl; locally crawled data always takes precedence.

### Open Knowledge Format codec

`demarkus okf` interoperates with [Open Knowledge Format (OKF) v0.1](https://github.com/GoogleCloudPlatform/knowledge-catalog) bundles: directories of markdown files with YAML frontmatter. A demarkus document is content-compatible with OKF; the codec moves whole bundles in and out of a world.

```bash
# Validate a bundle for OKF v0.1 conformance
demarkus okf validate ./my-bundle
demarkus okf validate --strict ./my-bundle   # warnings become failures

# Import a bundle into a world (sanitizes/caps metadata, rewrites links)
demarkus okf import ./my-bundle mark://localhost:6309/imported
demarkus okf import --dry-run ./my-bundle mark://localhost:6309/imported

# Export a world subtree back out as a conformant bundle
demarkus okf export mark://localhost:6309/imported ./exported
```

`import`/`export` accept `-auth <token>` and `--insecure` like the other write commands. Import is an upsert: re-importing an unchanged bundle creates no new versions (content-hash dedup). See [Markdown Reference → OKF compatibility](../reference/markdown.md#open-knowledge-format-okf-compatibility).

## TUI (`demarkus-tui`)

The TUI provides an interactive markdown browser with history, link navigation, and a document graph view.

```bash
demarkus-tui --insecure mark://localhost:6309/index.md
```

### Keyboard highlights

- `Tab`: cycle links
- `Enter`: follow selected link
- `[` / `]`: back / forward
- `d`: document graph view (loads stored graph instantly, live crawl updates in background)
- `?`: help

### External links

The TUI follows `mark://` links internally. Links to `http`, `https`, `gemini`, and `mailto` URLs are opened in the system's default handler (`open` / `xdg-open` / `rundll32`). Selected external links are shown in the status bar with an `↗` prefix to distinguish them from internal navigation.

Configure the scheme allowlist with `-external-links`:

```bash
# default: open http/https/gemini/mailto
demarkus-tui mark://localhost:6309/

# only http and https
demarkus-tui -external-links "http,https" mark://localhost:6309/

# disable external launching entirely; non-mark links report an error
demarkus-tui -external-links "" mark://localhost:6309/
```

URLs are passed as argv arguments to the system handler; no shell is invoked. Schemes not in the allowlist (including `file:`, `javascript:`, `data:`) are rejected before any process is spawned. See [Security Model](../security/index.md#external-links-in-the-tui) for details.

## MCP (`demarkus-mcp`)

The MCP server exposes Demarkus as tools for LLM agents over stdio.

```bash
demarkus-mcp -host mark://localhost:6309 -insecure
```

When `-host` is provided, tools accept bare paths (e.g. `/index.md`) instead of full URLs.

The 15 registered tools are `mark_fetch`, `mark_list`, `mark_explore`, `mark_versions`, `mark_lookup`, `mark_publish`, `mark_append`, `mark_archive`, `mark_discover`, `mark_graph`, `mark_backlinks`, `mark_graph_export`, `mark_graph_publish`, `mark_index`, and `mark_resolve`. Brokered knowledge systems add `mark_worlds` (the world directory) and `mark_lookup_all` (universe-wide catalog lookup), for 17 in total. `mark_list` returns one bounded page; when `complete` is false, pass `next-cursor` back as `cursor`. `mark_lookup` looks up documents by subject against the server's catalog (declared tags + title, ranked by importance). The `mark_graph` tool crawls and persists the document graph; `mark_backlinks` queries it for reverse links. `mark_graph_export` renders the graph as publishable markdown; `mark_graph_publish` exports and publishes in one step so other agents can discover the topology without recrawling.

## Accessing Private Servers

All three clients support read authentication for servers with protected paths. Tokens are resolved in this order: explicit token flag (CLI: `-auth`, MCP: `-token`; TUI has no token flag) > `DEMARKUS_AUTH` env var > stored token from `~/.mark/tokens.toml`.

### Store a token once

```bash
demarkus token add mark://private.example:6309 <raw-token>
```

After this, all clients automatically use the token when accessing that server; no extra flags needed.

### CLI

```bash
# Uses stored token automatically
demarkus --insecure mark://private.example:6309/internal/doc.md

# Or pass explicitly
demarkus --insecure -auth <raw-token> mark://private.example:6309/internal/doc.md
```

The `edit`, `graph`, `info`, and `bookmark` subcommands also use stored tokens.

### TUI

```bash
demarkus-tui --insecure mark://private.example:6309/internal/doc.md
```

The TUI loads tokens fresh on each navigation, so tokens added via CLI while the TUI is running are picked up immediately.

### MCP

```bash
# Single token for all hosts
demarkus-mcp -host mark://private.example:6309 -token <raw-token> -insecure

# Or rely on stored tokens (per-host resolution)
demarkus-mcp -host mark://private.example:6309 -insecure
```

The MCP server resolves tokens per-host: the `-token` flag takes precedence, then `DEMARKUS_AUTH`, then the stored token for the target host.

## Related Tools

- [Federation Crawler](../tools/agent.md): discover servers and build hash indexes
- [Token Tooling](../tools/index.md)

## Next Steps

- [Run a Server](../server/index.md)
- [Deploy with TLS](../deployment/index.md)
- [Configuration Reference](../reference/index.md)
