# Use the Clients

This section covers the three Demarkus client tools: the CLI (`demarkus`), the TUI browser (`demarkus-tui`), and the MCP server (`demarkus-mcp`). Each tool is built on the same Mark Protocol client layer and can be used together.

## Overview

- **CLI** (`demarkus`) — scripting, automation, and publishing
- **TUI** (`demarkus-tui`) — interactive terminal browser with link navigation
- **MCP** (`demarkus-mcp`) — exposes Mark Protocol as tools for LLM agents

If you're new, start with the CLI and confirm you can fetch a document.

## CLI (`demarkus`)

The CLI supports `FETCH`, `LIST`, `VERSIONS`, and `PUBLISH`, plus `edit` and `graph` subcommands.

### Common commands

```bash
# Fetch a document
demarkus --insecure mark://localhost:6309/index.md

# List a directory
demarkus --insecure -X LIST mark://localhost:6309/

# Publish a document
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/hello.md -body "# Hello"

# View version history
demarkus --insecure -X VERSIONS mark://localhost:6309/hello.md

# Fetch a specific version
demarkus --insecure mark://localhost:6309/hello.md/v1
```

### Edit a document

Opens a document in `$EDITOR` (falls back to `vi`), then publishes changes on save. If the document doesn't exist, creates a new one. Empty documents are rejected.

```bash
# Edit an existing document
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/hello.md

# Create a new document (opens empty editor)
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/new-doc.md
```

### Graph crawl

```bash
demarkus --insecure graph -depth 3 mark://localhost:6309/index.md
```

## TUI (`demarkus-tui`)

The TUI provides an interactive markdown browser with history, link navigation, and a document graph view.

```bash
demarkus-tui --insecure mark://localhost:6309/index.md
```

### Keyboard highlights

- `Tab` — cycle links
- `Enter` — follow selected link
- `[` / `]` — back / forward
- `d` — document graph view
- `?` — help

## MCP (`demarkus-mcp`)

The MCP server exposes Demarkus as tools for LLM agents over stdio.

```bash
demarkus-mcp -host mark://localhost:6309 -insecure
```

When `-host` is provided, tools accept bare paths (e.g. `/index.md`) instead of full URLs.

## Related Tools

- [Token Tooling](../tools/index.md)

## Next Steps

- [Run a Server](../server/index.md)
- [Deploy with TLS](../deployment/index.md)
- [Configuration Reference](../reference/index.md)