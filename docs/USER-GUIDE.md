# Demarkus User Guide

Demarkus is a markdown-native document server built on QUIC. You create, update, and read documents through a simple protocol — every publish creates an immutable version with cryptographic integrity verification. This guide covers everything you need to run a server and use the client.

For the protocol specification, see [SPEC.md](SPEC.md). For design rationale, see [DESIGN.md](DESIGN.md).

## Installation

### Build from source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all
```

This produces three binaries:

| Binary | Location | Purpose |
|--------|----------|---------|
| `demarkus-server` | `server/bin/demarkus-server` | Serves documents |
| `demarkus-token` | `server/bin/demarkus-token` | Generates auth tokens (server-side tool) |
| `demarkus` | `client/bin/demarkus` | Reads and publishes documents |

### Pre-built binaries

Download from [GitHub Releases](https://github.com/latebit-io/demarkus/releases). The server and client are released separately.

## Getting Started

This walkthrough sets up a server and creates your first document. All commands are copy-pasteable.

### 1. Create a content directory and generate a token

```bash
mkdir /tmp/my-site

TOKEN=$(demarkus-token generate -paths "/*" -ops publish -tokens /tmp/tokens.toml)
```

The raw token is printed to stdout (captured in `$TOKEN`). The hashed entry is appended to `/tmp/tokens.toml`. The server never stores the raw token.

### 2. Start the server

```bash
demarkus-server -root /tmp/my-site -tokens /tmp/tokens.toml
```

You should see:

```
[INFO] tls: using self-signed dev certificate (set DEMARKUS_TLS_CERT and DEMARKUS_TLS_KEY for production)
[INFO] auth: loaded tokens from /tmp/tokens.toml
[INFO] demarkus-server listening on :6309 (root: /tmp/my-site, idle_timeout: 30s, request_timeout: 10s)
```

### 3. Create a document

In a separate terminal:

```bash
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/index.md \
  -body "# My Site

Welcome to my Demarkus site.

## Pages

- [About](about.md)
- [First Post](blog/hello-world.md)"
```

Output:

```
[created] version=1 modified=2026-02-19T14:30:00Z
```

### 4. Read it back

```bash
demarkus --insecure mark://localhost:6309/index.md
```

Output:

```
[ok] version=1 etag=a1b2c3... modified=2026-02-19T14:30:00Z
# My Site

Welcome to my Demarkus site.

## Pages

- [About](about.md)
- [First Post](blog/hello-world.md)
```

### 5. Create more pages

```bash
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/about.md \
  -body "# About

This site is served by Demarkus, a markdown-native document protocol."

demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/blog/hello-world.md \
  -body "# Hello World

This is the first post on my blog. Every version is immutable and
cryptographically chained to the previous one."
```

### 6. Update a document

```bash
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/index.md \
  -body "# My Site

Welcome to my Demarkus site. Updated with new content.

## Pages

- [About](about.md)
- [First Post](blog/hello-world.md)
- [Second Post](blog/updates.md)"
```

Output:

```
[created] version=2 modified=2026-02-19T14:35:00Z
```

The original version 1 is preserved. The document now has two versions.

### 7. View version history

```bash
demarkus --insecure -X VERSIONS mark://localhost:6309/index.md
```

Output:

```
[ok] chain-valid=true current=2 total=2

# Version History: /index.md

- [v2](/index.md/v2) - 2026-02-19T14:35:00Z
- [v1](/index.md/v1) - 2026-02-19T14:30:00Z
```

### 8. List the site

```bash
demarkus --insecure -X LIST mark://localhost:6309/
```

Output:

```
[ok] entries=3

# Index of /

- [about.md](about.md)
- [blog/](blog/)
- [index.md](index.md)
```

## Running the Server

### Minimum configuration

The only required setting is the content directory:

```bash
demarkus-server -root /srv/site
```

Without a tokens file, the server is read-only — all PUBLISH requests are denied with `not-permitted`. This is the secure default.

### Configuration reference

Flags override environment variables. Both methods work for all options except the three env-only settings.

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `-root` | `DEMARKUS_ROOT` | (required) | Content directory to serve |
| `-port` | `DEMARKUS_PORT` | `6309` | UDP port to listen on |
| `-tls-cert` | `DEMARKUS_TLS_CERT` | (none) | Path to TLS certificate PEM |
| `-tls-key` | `DEMARKUS_TLS_KEY` | (none) | Path to TLS private key PEM |
| `-tokens` | `DEMARKUS_TOKENS` | (none) | Path to tokens TOML file |
| — | `DEMARKUS_MAX_STREAMS` | `10` | Max concurrent QUIC streams |
| — | `DEMARKUS_IDLE_TIMEOUT` | `30s` | Connection idle timeout |
| — | `DEMARKUS_REQUEST_TIMEOUT` | `10s` | Per-request timeout |

Both `-tls-cert` and `-tls-key` must be provided together. Providing one without the other is a startup error.

### TLS: dev mode vs. production

**Dev mode** (no cert/key): The server generates a self-signed Ed25519 certificate in memory. Clients must use `--insecure` to skip TLS verification.

**Production mode**: Provide both `-tls-cert` and `-tls-key`. Certificates are served via a callback that supports live reload.

```bash
demarkus-server -root /srv/site -tokens /etc/demarkus/tokens.toml \
  -tls-cert /etc/letsencrypt/live/example.com/fullchain.pem \
  -tls-key /etc/letsencrypt/live/example.com/privkey.pem
```

See the [README](../README.md) for a full Let's Encrypt setup walkthrough.

### Certificate reload and graceful shutdown

- **SIGHUP**: Reloads TLS certificates from disk without dropping connections. Useful after Let's Encrypt renewals.
- **SIGINT / SIGTERM**: Stops accepting new connections, drains in-flight requests (up to 10 seconds), then exits.

## Using the Client

### URL format

```
mark://hostname[:port]/path
```

Port defaults to `6309`. Examples:

```
mark://localhost:6309/index.md
mark://example.com/docs/guide.md
mark://example.com/
```

### Reading documents (FETCH)

FETCH is the default verb:

```bash
demarkus --insecure mark://localhost:6309/index.md
```

Output format: `[status] key=value...\n<body>`

```
[ok] version=2 etag=d4e5f6... modified=2026-02-19T14:35:00Z
# My Site
...
```

### Listing directories (LIST)

```bash
demarkus --insecure -X LIST mark://localhost:6309/
```

```
[ok] entries=3

# Index of /

- [about.md](about.md)
- [blog/](blog/)
- [index.md](index.md)
```

The `versions/` directory and dot-files are hidden. Maximum 1000 entries per directory.

### Publishing documents (PUBLISH)

Requires an auth token. Body from `-body` flag or stdin:

```bash
# Inline (short content)
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/doc.md \
  -body "# Document content"

# From a local file (most common for real content)
cat article.md | demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/article.md
```

Piping from a file is the most practical way to create and update documents. Write your markdown locally with any editor, then publish:

```bash
# Write a post in your editor
vim ~/drafts/new-post.md

# Publish it
cat ~/drafts/new-post.md | demarkus --insecure -X PUBLISH -auth $TOKEN \
  mark://localhost:6309/blog/new-post.md

# Edit and update (creates version 2)
vim ~/drafts/new-post.md
cat ~/drafts/new-post.md | demarkus --insecure -X PUBLISH -auth $TOKEN \
  mark://localhost:6309/blog/new-post.md
```

Maximum document size: 10 MB.

### Version history (VERSIONS)

```bash
demarkus --insecure -X VERSIONS mark://localhost:6309/index.md
```

```
[ok] chain-valid=true current=2 total=2

# Version History: /index.md

- [v2](/index.md/v2) - 2026-02-19T14:35:00Z
- [v1](/index.md/v1) - 2026-02-19T14:30:00Z
```

- `chain-valid=true` — the SHA-256 hash chain is intact; no version has been tampered with.
- `chain-valid=false` — a version was modified on disk. `chain-error` describes which link failed.

### Fetching a specific version

Append `/vN` to the path:

```bash
demarkus --insecure mark://localhost:6309/index.md/v1
```

```
[ok] version=1 current-version=2 modified=2026-02-19T14:30:00Z
# My Site

Welcome to my Demarkus site.
...
```

The `current-version` field tells you the latest version, so you can see whether you're looking at a historical snapshot.

### Caching

The client caches FETCH and LIST responses by default at `~/.mark/cache/`. Subsequent requests use conditional headers (`if-none-match`, `if-modified-since`). When the server responds with `not-modified`, the client serves the cached copy and shows `(cached)`:

```
[ok] version=2 etag=d4e5f6... modified=2026-02-19T14:35:00Z (cached)
```

PUBLISH and VERSIONS responses are never cached.

```bash
# Disable caching
demarkus --insecure --no-cache mark://localhost:6309/index.md

# Custom cache directory
demarkus --insecure --cache-dir /tmp/mark-cache mark://localhost:6309/index.md
```

### Health check

```bash
demarkus --insecure mark://localhost:6309/health
```

```
[ok]
# Health Check

Server is healthy.
```

No auth required. Does not touch the content directory.

## Authentication

### How tokens work

Tokens are capability-based: they grant specific operations on specific path patterns. A token is a random 64-character hex string. The server stores only the SHA-256 hash — the raw token is shown once at generation time and never stored on disk.

If you lose a token, generate a new one. There is no recovery.

### Generating tokens

```bash
demarkus-token generate [-paths PATTERNS] [-ops OPERATIONS] [-tokens FILE]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-paths` | `/*` | Comma-separated glob patterns |
| `-ops` | `publish` | Comma-separated operations |
| `-tokens` | (none) | File to append to; created if absent |

The raw token goes to stdout. All other output goes to stderr, making it pipe-safe:

```bash
TOKEN=$(demarkus-token generate -paths "/*" -ops publish -tokens tokens.toml)
echo "Save this token: $TOKEN"
```

Without `-tokens`, the tool prints the TOML entry to stderr for manual addition:

```bash
demarkus-token generate -paths "/docs/*" -ops publish
# stderr: Add this to your tokens.toml under [tokens]:
# stderr: "sha256-a1b2c3..." = { paths = ["/docs/*"], operations = ["publish"] }
# stdout: <raw-token>
```

### The tokens.toml format

```toml
[tokens]
"sha256-c6d9b584..." = { paths = ["/*"], operations = ["publish"] }
"sha256-e5f6a7b8..." = { paths = ["/docs/*"], operations = ["publish"] }
```

The file is loaded once at server startup. To add a new token, generate it and restart the server.

Path patterns use glob syntax: `*` matches within a single directory level, `?` matches one character. Recursive matching (`**`) is not yet supported.

> **Note**: The `expires` field is parsed from TOML but not yet enforced. Do not rely on token expiration.

### Sending tokens from the client

Flag takes precedence over environment variable:

```bash
# Flag
demarkus --insecure -X PUBLISH -auth <raw-token> mark://host/path -body "..."

# Environment variable
export DEMARKUS_AUTH=<raw-token>
demarkus --insecure -X PUBLISH mark://host/path -body "..."
```

### Error responses

| Status | Meaning |
|--------|---------|
| `unauthorized` | No token sent, or token not recognized |
| `not-permitted` | Token valid but lacks permission for this path/operation |
| `not-permitted` | No tokens file configured on the server (publishing disabled) |

FETCH and LIST do not require a token.

## Real-World Workflows

### Setting up a blog

```bash
# Set up
mkdir /srv/blog
TOKEN=$(demarkus-token generate -paths "/*" -ops publish -tokens /srv/blog-tokens.toml)
demarkus-server -root /srv/blog -tokens /srv/blog-tokens.toml &

# Create the index
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/index.md \
  -body "# My Blog

## Recent Posts

- [Why I Switched to Demarkus](posts/why-demarkus.md)
- [Getting Started with QUIC](posts/quic-intro.md)"

# Create posts
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/posts/why-demarkus.md \
  -body "# Why I Switched to Demarkus

Every version of every post is permanent. No silent edits, no deleted history.
Readers can verify that what they see is what was published."

demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/posts/quic-intro.md \
  -body "# Getting Started with QUIC

QUIC is a transport protocol built on UDP. It provides encrypted connections
with lower latency than TCP+TLS."

# Browse
demarkus --insecure -X LIST mark://localhost:6309/
demarkus --insecure -X LIST mark://localhost:6309/posts/
demarkus --insecure mark://localhost:6309/posts/why-demarkus.md
```

### Updating existing content

The simplest workflow: keep your markdown files locally and publish updates by piping them in.

```bash
# Your local file is the source of truth
vim ~/site/posts/why-demarkus.md

# Publish the update (creates the next version)
cat ~/site/posts/why-demarkus.md | demarkus --insecure -X PUBLISH -auth $TOKEN \
  mark://localhost:6309/posts/why-demarkus.md
```

Output: `[created] version=2 modified=2026-02-19T15:00:00Z`

Every publish creates a new version. The original version 1 is still accessible:

```bash
demarkus --insecure mark://localhost:6309/posts/why-demarkus.md/v1
```

You can also publish an entire directory of local files at once — see [Scripting with demarkus](#scripting-with-demarkus) below.

### Building a documentation site

Nested directories are created automatically on first publish:

```bash
# API docs
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/docs/api/authentication.md \
  -body "# Authentication API

## Endpoints

### Generate Token
..."

demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/docs/api/documents.md \
  -body "# Documents API

## Endpoints

### Fetch Document
..."

# Guides
demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/docs/guides/quickstart.md \
  -body "# Quickstart Guide

Follow these steps to get started..."

# Browse the structure
demarkus --insecure -X LIST mark://localhost:6309/docs/
demarkus --insecure -X LIST mark://localhost:6309/docs/api/
```

### Verifying content integrity

Check that no published version has been tampered with:

```bash
demarkus --insecure -X VERSIONS mark://localhost:6309/posts/why-demarkus.md
```

```
[ok] chain-valid=true current=2 total=2
...
```

If `chain-valid=false`, the `chain-error` field identifies the problem:

```
[ok] chain-valid=false chain-error="v2: previous-hash mismatch" current=2 total=2
```

This means version 2's recorded hash of version 1 doesn't match the actual content of version 1 on disk — someone or something modified a version file directly.

Compare versions to see what changed:

```bash
# Fetch both versions
demarkus --insecure mark://localhost:6309/posts/why-demarkus.md/v1 > /tmp/v1.md
demarkus --insecure mark://localhost:6309/posts/why-demarkus.md/v2 > /tmp/v2.md

# Diff them
diff /tmp/v1.md /tmp/v2.md
```

### Scripting with demarkus

**Batch upload a directory of markdown files**:

```bash
TOKEN=$(demarkus-token generate -paths "/*" -ops publish -tokens tokens.toml)

for file in content/*.md; do
  name=$(basename "$file")
  cat "$file" | demarkus --insecure -X PUBLISH -auth $TOKEN \
    mark://localhost:6309/"$name"
done
```

**Upload nested directories**:

```bash
find content -name "*.md" | while read file; do
  # Strip the "content/" prefix to get the server path
  path="${file#content/}"
  cat "$file" | demarkus --insecure -X PUBLISH -auth $TOKEN \
    mark://localhost:6309/"$path"
done
```

**Capture a token for later use**:

```bash
# Generate and save
TOKEN=$(demarkus-token generate -paths "/blog/*" -ops publish -tokens tokens.toml)
echo "$TOKEN" > ~/.demarkus-blog-token
chmod 600 ~/.demarkus-blog-token

# Use later
export DEMARKUS_AUTH=$(cat ~/.demarkus-blog-token)
demarkus --insecure -X PUBLISH mark://localhost:6309/blog/new-post.md -body "# New Post"
```

## Content and Versioning

### Publish creates immutable versions

Every PUBLISH creates a new version number. Version 1 is the genesis. Each subsequent publish increments the version. Published versions are permanent — there is no way to delete or modify a version through the protocol.

If content needs correction, publish a new version. The old version remains accessible at its `/vN` path.

### Hash chain integrity

Each version file (except v1) records the SHA-256 hash of the previous version's raw bytes. This creates a chain: if any past version is altered on disk, the chain breaks and VERSIONS reports `chain-valid=false`.

This is verified on every VERSIONS request. It provides cryptographic proof that history has not been rewritten.

### On-disk layout

The store uses a `versions/` subdirectory alongside each document:

```
content-root/
  index.md              <- symlink to versions/index.md.v2
  about.md              <- symlink to versions/about.md.v1
  versions/
    index.md.v1
    index.md.v2
    about.md.v1
  blog/
    hello-world.md      <- symlink to versions/hello-world.md.v1
    versions/
      hello-world.md.v1
```

The symlink always points to the latest version. It is updated atomically on each publish, so readers never see a missing file.

### Versioned-only serving

The server only serves documents that have a `versions/` directory with at least one version file. If you copy a plain markdown file directly into the content directory, the server returns `not-found`.

All content must enter the system through PUBLISH. This ensures every served document has a verifiable hash chain from the start.

## Output Reference

### Format

The client always prints:

```
[status] key1=value1 key2=value2 (cached)
<body>
```

Status and metadata are on one line. Body follows on subsequent lines. If the body is empty, there is no additional output after the status line.

### Status values

| Status | When it appears |
|--------|----------------|
| `ok` | Successful FETCH, LIST, VERSIONS |
| `created` | Successful PUBLISH |
| `not-found` | Document or path does not exist |
| `unauthorized` | No token or unrecognized token |
| `not-permitted` | Insufficient permissions or publishing disabled |
| `server-error` | Internal server error |

The client also handles `not-modified` internally — it resolves to the cached response and shows `(cached)`.

### Metadata fields per verb

**FETCH**:
- `version` — version number
- `modified` — RFC 3339 timestamp
- `etag` — SHA-256 hex of content

**FETCH (specific version /vN)**:
- `version` — the requested version number
- `modified` — RFC 3339 timestamp of that version
- `current-version` — the latest version number

**LIST**:
- `entries` — count of directory entries

**VERSIONS**:
- `total` — total number of versions
- `current` — latest version number
- `chain-valid` — `true` or `false`
- `chain-error` — present only when `chain-valid=false`

**PUBLISH (success)**:
- `version` — the version number created
- `modified` — RFC 3339 timestamp

## Quick Reference

### Server

```
demarkus-server [-root DIR] [-port PORT] [-tls-cert FILE] [-tls-key FILE] [-tokens FILE]
```

| Flag | Env Var | Default |
|------|---------|---------|
| `-root` | `DEMARKUS_ROOT` | (required) |
| `-port` | `DEMARKUS_PORT` | `6309` |
| `-tls-cert` | `DEMARKUS_TLS_CERT` | (self-signed) |
| `-tls-key` | `DEMARKUS_TLS_KEY` | (self-signed) |
| `-tokens` | `DEMARKUS_TOKENS` | (publishing disabled) |
| — | `DEMARKUS_MAX_STREAMS` | `10` |
| — | `DEMARKUS_IDLE_TIMEOUT` | `30s` |
| — | `DEMARKUS_REQUEST_TIMEOUT` | `10s` |

### Client

```
demarkus [-X VERB] [-body TEXT] [-auth TOKEN] [--insecure] [--no-cache] [--cache-dir DIR] mark://host[:port]/path
```

| Flag | Env Var | Default |
|------|---------|---------|
| `-X` | — | `FETCH` |
| `-body` | — | (stdin for PUBLISH) |
| `-auth` | `DEMARKUS_AUTH` | — |
| `--insecure` | — | `false` |
| `--no-cache` | — | `false` |
| `--cache-dir` | `DEMARKUS_CACHE_DIR` | `~/.mark/cache` |

### Token tool

```
demarkus-token generate [-paths PATTERNS] [-ops OPERATIONS] [-tokens FILE]
```

| Flag | Default |
|------|---------|
| `-paths` | `/*` |
| `-ops` | `publish` |
| `-tokens` | (prints to stderr) |
