# Tools

This section covers the supporting tools that ship with Demarkus. Today that primarily means **token generation** for capability-based authentication.

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
```

### Run the server with tokens

```bash
./server/bin/demarkus-server -root /srv/site -tokens /path/to/tokens.toml
```

### Use the token from the client

```bash
./client/bin/demarkus --insecure -X PUBLISH -auth <raw-token> mark://localhost:6309/hello.md -body "# Hello World"
```

You can also set it once via environment variable:

```bash
export DEMARKUS_AUTH=<raw-token>
./client/bin/demarkus --insecure -X PUBLISH mark://localhost:6309/hello.md -body "# Hello World"
```

### Client-side token management

The CLI can store tokens per-server so you don't need to pass `-auth` every time:

```bash
# Store a token for a server
demarkus token add mark://localhost:6309 <raw-token>

# List servers with stored tokens
demarkus token list

# Remove a stored token
demarkus token remove mark://localhost:6309
```

Stored tokens are saved to `~/.mark/tokens.toml` (permissions `0600`). When making requests, the CLI resolves tokens in order: `-auth` flag > `DEMARKUS_AUTH` env var > stored token for the host.

## Best Practices

- **Store tokens securely** (password manager or encrypted secrets store).
- **Scope tokens** to the minimum paths and operations required.
- **Rotate tokens** regularly for long‑lived deployments.
- **Do not commit** `tokens.toml` to source control.

## Related

- [Run a Server](../server/index.md)
- [Configuration Reference](../reference/index.md)
- [Install & Build](../install/index.md)