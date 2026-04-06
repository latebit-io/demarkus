# Tools

This section covers the supporting tools that ship with Demarkus: the **federation crawler** for discovering servers and building indexes, and **token generation** for capability-based authentication.

## Federation Crawler (`demarkus-agent`)

The federation crawler discovers Mark Protocol servers, collects content hashes, and publishes indexes to hubs. It runs the core federation loop: seed → crawl → hash → index → repeat.

See [Federation Crawler](./agent.md) for full documentation.

### Quick start

```bash
# Single crawl
demarkus-agent crawl -seeds "mark://localhost:6309" -insecure -v

# Daemon mode with config
demarkus-agent daemon -config fedcrawl.toml -insecure
```

## Token Generation (`demarkus-token`)

The server is secure by default and **denies writes** unless you configure a tokens file. Tokens are capability-based: they grant operations on path patterns, not identities. The server stores **only the SHA-256 hash** of tokens, never the raw secret.

### Generate a token (publish access)

```bash
./server/bin/demarkus-token generate -paths "/*" -ops publish -tokens tokens.toml
```

- The **raw token** is printed once (give it to the client).
- The **hashed token** is appended under `[tokens]` in `tokens.toml`.

### Scope tokens by path

```bash
# Publish-only to /docs/*
./server/bin/demarkus-token generate -paths "/docs/*" -ops publish -tokens tokens.toml

# Read + publish to everything
./server/bin/demarkus-token generate -paths "/*" -ops "read,publish" -tokens tokens.toml

# Read-only access to private paths
./server/bin/demarkus-token generate -paths "/internal/**" -ops read -tokens tokens.toml
```

### Read tokens (private paths)

By default all paths are public. To protect paths, create a token with the `read` operation:

```bash
# Protect /internal/** — requires a read token for FETCH, LIST, VERSIONS
./server/bin/demarkus-token generate -paths "/internal/**" -ops read -tokens tokens.toml

# Full private server — protect everything
./server/bin/demarkus-token generate -paths "/**" -ops "read,publish" -tokens tokens.toml
```

Paths not covered by any read token remain public. The well-known manifest (`/.well-known/agent-manifest.md`) is always accessible.

### Run the server with tokens

```bash
./server/bin/demarkus-server -root /srv/site -tokens /path/to/tokens.toml
```

### Use the token from the client

When accessing protected paths, clients send a token for reads (FETCH, LIST, VERSIONS) and writes (PUBLISH, APPEND, ARCHIVE). The server always allows unauthenticated access to well-known public endpoints like `/.well-known/agent-manifest.md`. The server only authorizes operations permitted by that token's configured capabilities (`ops`). Token resolution follows this precedence: explicit token flag (`-auth` for the CLI, `-token` for MCP) > `DEMARKUS_AUTH` env var > stored token for the host. The TUI has no token flag and relies on `DEMARKUS_AUTH` and stored tokens.

#### Writing to a server

```bash
demarkus --insecure -X PUBLISH -auth <raw-token> mark://localhost:6309/hello.md -body "# Hello World"
```

#### Reading from a private server

```bash
# Explicit token
demarkus --insecure -auth <raw-token> mark://localhost:6309/internal/doc.md
```

```bash
# Via environment variable
export DEMARKUS_AUTH=<raw-token>
demarkus --insecure mark://localhost:6309/internal/doc.md
```

```bash
# Via stored token (see below) — no flags or env needed
demarkus --insecure mark://localhost:6309/internal/doc.md
```

The TUI and MCP server also use stored tokens automatically:

```bash
# TUI picks up stored tokens for each host it navigates to
demarkus-tui --insecure mark://localhost:6309/internal/doc.md

# MCP uses -token flag, DEMARKUS_AUTH, or stored tokens
demarkus-mcp -host mark://localhost:6309 -token <raw-token> -insecure
```

### Client-side token management

The CLI can store tokens per-server so you don't need to pass `-auth` every time. Stored tokens are used automatically by all three clients (CLI, TUI, MCP) for protected reads and writes. Public paths remain accessible without a token.

```bash
# Store a token for a server
demarkus token add mark://localhost:6309 <raw-token>

# List servers with stored tokens
demarkus token list

# Remove a stored token
demarkus token remove mark://localhost:6309
```

Stored tokens are saved to `~/.mark/tokens.toml` (permissions `0600`).

## Best Practices

- **Store tokens securely** (password manager or encrypted secrets store).
- **Scope tokens** to the minimum paths and operations required.
- **Rotate tokens** regularly for long‑lived deployments.
- **Do not commit** `tokens.toml` to source control.

## Related

- [Run a Server](../server/index.md)
- [Configuration Reference](../reference/index.md)
- [Install & Build](../install/index.md)
