# Tools

This section covers the supporting tools that ship with Demarkus: the **federation crawler** for discovering servers and building indexes, **token generation** for capability-based authentication, and **direct-to-store publishing** for read-only server installs.

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

### Labels — give every token a meaningful name

Every token is identified by a **label** that you pick at generation time. Labels are the primary way to reason about tokens in Demarkus — they show up in audit logs on every write operation, they're how you revoke a token later (`demarkus-token revoke -label NAME`), and they live alongside the hashed token in `tokens.toml` as `[tokens.<label>]`.

Pick a label that tells you (or your future self) **who or what uses this token and where**. Good labels: `fritz-laptop`, `ci-deploy`, `agent-crawler`, `claude-code-plugin`, `obsidian-fritz-phone`. Bad labels: `token1`, `t`, `xxx`.

A descriptive label is how you'll know which entry to revoke when a device is lost or a service is decommissioned.

### Generate a token (publish access)

```bash
./server/bin/demarkus-token generate -label my-laptop -paths "/*" -ops publish -tokens tokens.toml
```

- `-label` is required.
- The **raw token** is printed once (give it to the client).
- The **hashed token** is appended under `[tokens.<label>]` in `tokens.toml`.
- Every subsequent operation authenticated with this token logs `token_label=my-laptop` in the server audit stream.

### Scope tokens by path

```bash
# Publish-only to /docs/*
./server/bin/demarkus-token generate -label docs-writer -paths "/docs/*" -ops publish -tokens tokens.toml

# Read + publish to everything
./server/bin/demarkus-token generate -label admin -paths "/*" -ops "read,publish" -tokens tokens.toml

# Read-only access to private paths
./server/bin/demarkus-token generate -label internal-reader -paths "/internal/**" -ops read -tokens tokens.toml
```

### Join URLs (`demarkus-token join`)

A join URL packs the host and token into one paste-able string, so onboarding a device or person is a single step instead of "here's a token, now assemble the flags":

```bash
# Compose with generate: mint a scoped token, wrap it in a join URL
./server/bin/demarkus-token generate -label phone -paths "/*" -ops publish -tokens tokens.toml \
  | ./server/bin/demarkus-token join -host kb.example.com
# -> mark://kb.example.com#token=...
```

The recipient pastes it once:

- Claude Code (demarkus-memory plugin): `/soul-join <join-url>` (add `--insecure` for a self-signed server)
- CLI: `demarkus join '<join-url>'` (quote it; the shell would strip the `#fragment`)

The credential rides in the URL fragment, which never appears in any request; still treat a join URL like the token it carries, and send it over a private channel.

`install.sh` prints a ready-made join URL for the freshly installed server at the end of a server install.

### Read tokens (private paths)

By default all paths are public. To protect paths, create a token with the `read` operation:

```bash
# Protect /internal/** — requires a read token for FETCH, LIST, VERSIONS
./server/bin/demarkus-token generate -label internal-reader -paths "/internal/**" -ops read -tokens tokens.toml

# Full private server — protect everything
./server/bin/demarkus-token generate -label team-member -paths "/**" -ops "read,publish" -tokens tokens.toml
```

Paths not covered by any read token remain public. The well-known manifest (`/.well-known/agent-manifest.md`) is always accessible.

### Run the server with tokens

```bash
./server/bin/demarkus-server -root /srv/site -tokens /path/to/tokens.toml
```

### Use the token from the client

When accessing protected paths, clients send a token for reads (FETCH, LIST, VERSIONS; LOOKUP filters protected docs out of its results rather than rejecting) and writes (PUBLISH, APPEND, ARCHIVE). The server always allows unauthenticated access to well-known public endpoints like `/.well-known/agent-manifest.md`. The server only authorizes operations permitted by that token's configured capabilities (`ops`). Token resolution follows this precedence: explicit token flag (`-auth` for the CLI, `-token` for MCP) > `DEMARKUS_AUTH` env var > stored token for the host. The TUI has no token flag and relies on `DEMARKUS_AUTH` and stored tokens.

#### Writing to a server

```bash
demarkus --insecure -X PUBLISH -auth <raw-token> -body "# Hello World" mark://localhost:6309/hello.md
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

- **Use descriptive labels.** `fritz-laptop`, `ci-deploy`, `claude-code-plugin` — never `token1`. Labels are what you'll look up when revoking and what shows up in audit logs.
- **One label per device, service, or agent.** Don't share a token across two machines; give each its own entry so a loss only revokes one.
- **Store tokens securely** (password manager or encrypted secrets store).
- **Scope tokens** to the minimum paths and operations required.
- **Rotate tokens** regularly for long‑lived deployments — revoke the old label and generate a new one.
- **Do not commit** `tokens.toml` to source control.

## Direct-to-Store Publish (`demarkus-publish`)

`demarkus-publish` writes a document directly to a server's versioned content store on disk, bypassing the QUIC protocol. It is designed for two scenarios:

1. **Read-only server installs** — when `demarkus-server` runs with `-read-only` (e.g. the chroot hardened deployment), the server cannot accept PUBLISH/APPEND/ARCHIVE. `demarkus-publish` runs as a separate process with filesystem write access and produces the same versioned store layout the server reads.
2. **Local batch publishing** — scripting or migration scenarios where spinning up a server and sending QUIC requests is overkill.

Full versioning is preserved because `demarkus-publish` shares the store logic with the server (`server/internal/store`). Each write creates a new immutable version.

### Flags

- `-root` — content directory (falls back to `DEMARKUS_ROOT` env var, required)
- `-path` — document path starting with `/` (e.g. `/index.md`, required)
- `-body` — document content (reads stdin if omitted)

### Publish inline

```bash
demarkus-publish -root /srv/demarkus/content -path /index.md -body "# Hello"
```

### Publish from a file (via stdin)

```bash
demarkus-publish -root /srv/demarkus/content -path /notes/2026-04-24.md < notes.md
```

Or pipe:

```bash
cat notes.md | demarkus-publish -root /srv/demarkus/content -path /notes/2026-04-24.md
```

### Behavior

- On success: prints `published <path> v<N>` to stderr.
- When the new content matches the current version: prints `unchanged (v<N>)` and exits 0 (no new version).
- Archive and append operations are not supported by this tool — use the `demarkus` CLI against a writable server for those.

See [Security Model → Read-Only Mode](../security/index.md#read-only-mode-maximum-lockdown) for the chroot deployment pattern that pairs with this tool.

## Related

- [Run a Server](../server/index.md)
- [Configuration Reference](../reference/index.md)
- [Install & Build](../install/index.md)
