# Federation Crawler (`demarkus-agent`)

The federation crawler discovers Mark Protocol servers, collects content hashes, and publishes indexes to hubs. It's a Go binary that handles the mechanical work of crawling — no LLM required.

## Overview

The crawler implements the core federation loop:

1. **Seed** — start from configured servers
2. **Crawl** — follow `mark://` links, discover new servers
3. **Hash** — collect content hashes from every document
4. **Index** — publish hash indexes to configured hubs
5. **Repeat** — on a configurable schedule

## Installation

The `demarkus-agent` binary is built alongside other clients:

```bash
# Build from source
cd client && go build ./cmd/demarkus-agent

# Binary location
./client/bin/demarkus-agent
```

## Quick Start

### Single Crawl

Crawl a single server with inline configuration:

```bash
demarkus-agent crawl -seeds "mark://localhost:6309" -insecure -v
```

Output shows servers discovered, documents crawled, hashes collected, and any errors.

### With Config File

Create `fedcrawl.toml`:

```toml
seeds = ["mark://localhost:6309"]
hubs = ["mark://hub.example:6309"]

[endpoints."hub.example:6309"]
dial_address = "hub.internal:6309"
server_name = "hub.internal"

[crawl]
max_servers = 50
max_documents = 1000
workers = 5

[politeness]
request_delay = "100ms"

[schedule]
interval = "6h"
```

Run the crawl:

```bash
demarkus-agent crawl -config fedcrawl.toml -insecure -v
```

### Daemon Mode

Run continuously on a schedule:

```bash
demarkus-agent daemon -config fedcrawl.toml -insecure
```

The daemon runs an initial crawl immediately, then repeats at the configured interval.

## CLI Reference

### `crawl` — Single Crawl Run

```bash
demarkus-agent crawl [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | — | Path to TOML config file |
| `-seeds` | — | Comma-separated seed servers (overrides config) |
| `-state` | `~/.mark/fedcrawl.json` | Path to state file |
| `-insecure` | false | Skip TLS certificate verification |
| `-publish` | false | Publish indexes to hubs after crawl |
| `-per-server` | false | Publish per-server indexes (not aggregated) |
| `-v` | false | Verbose output |

### `daemon` — Scheduled Crawling

```bash
demarkus-agent daemon -config <path> [options]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | — | Path to TOML config file (required) |
| `-seeds` | — | Comma-separated seed servers (overrides config) |
| `-state` | `~/.mark/fedcrawl.json` | Path to state file |
| `-insecure` | false | Skip TLS certificate verification |
| `-publish` | false | Publish indexes to hubs after each crawl |
| `-per-server` | false | Publish per-server indexes (not aggregated) |

Daemon mode requires a config file with `schedule.interval` set.

### `version` — Version Info

```bash
demarkus-agent version
```

## Configuration

Configuration is via TOML file. All fields have sensible defaults.

### Top-Level Fields

```toml
seeds = ["mark://server1:6309", "mark://server2:6309"]
hubs = ["mark://hub.example:6309"]
```

- `seeds` — Initial servers to crawl (required unless `-seeds` flag provided)
- `hubs` — Servers to publish hash indexes to

Hubs are publish-only unless also listed in `seeds`. Links to a publish-only hub
remain graph edges but do not enqueue the hub's generated artifacts for crawling.
The crawler hashes `/graph.md` and the `/graph/` snapshot namespace, but does not
ingest their generated links as authored edges or discovery targets. Graph
exports are projections, not sources.

### Explicit Transport Endpoints

An endpoint override separates stable Mark identity from deployment routing:

```toml
[endpoints."team-a"]
dial_address = "shared.internal:6309"
server_name = "team-a.internal"
```

The table key remains the authority used by links, graph nodes, indexes, state,
and token lookup. `dial_address` only opens the socket. `server_name` selects the
virtual server through TLS SNI and certificate verification; when omitted it
defaults to the logical authority hostname.

### `[crawl]` Section

```toml
[crawl]
max_servers = 50      # Max servers to discover
max_documents = 1000  # Max documents per crawl run
workers = 5           # Concurrent fetch goroutines
```

### `[schedule]` Section

```toml
[schedule]
interval = "6h"  # Time between crawl runs (daemon mode)
```

Interval must be > 0 for daemon mode. Use `1h`, `30m`, etc.

### `[politeness]` Section

```toml
[politeness]
request_delay = "100ms"           # Delay between requests to same server
per_server_concurrency = 2        # Max concurrent requests per server (future)
```

## State Persistence

The crawler maintains state in `~/.mark/fedcrawl.json`:

- **Visited URLs** — etag, status, content hash for each document
- **Known servers** — when discovered, last crawled, document count

State enables:
- Conditional fetch (skip unchanged documents)
- Resume after interruption
- Track crawl coverage over time

To start fresh, delete the state file:

```bash
rm ~/.mark/fedcrawl.json
```

## Hub Index Publishing

When `-publish` is set, the crawler publishes hash indexes to configured hubs.

### Aggregated Index

By default, the crawler publishes one logical index at `/index.md` containing entries from all servers:

```bash
demarkus-agent crawl -config fedcrawl.toml -publish -insecure
```

### Per-Server Indexes

With `-per-server`, publishes a separate index for each discovered server:

```bash
demarkus-agent crawl -config fedcrawl.toml -publish -per-server -insecure
```

Indexes are published to `/index/<host>.md` on each hub.

Each logical index path is a small manifest. Entries are stored in hash-prefix shards under a sibling `.shards/a/` or `.shards/b/` directory. The agent stages and verifies the inactive slot before publishing the manifest, so readers see either the old complete generation or the new complete generation. Existing single-document indexes are converted automatically on their next publication.

The agent needs read access and publish access to the manifest and its `.shards` subtree; read access may be public. Upgrade index readers and writers together before enabling this publisher against an existing legacy index. Configured retention applies to graph documents and index manifests. Version-pinned shard history remains unpruned so retained manifests cannot acquire dangling references. Operators should monitor `.shards` growth until reference-aware garbage collection is available; a fixed shard window is unsafe because failed or concurrent staging can advance shard versions without advancing the manifest.

### Content-Addressed Discovery

Published indexes enable content-addressed fetching. Resolution reads all shards matching the requested hash prefix, including every deterministic overflow part:

```bash
# Resolve content by hash (requires hub with index)
demarkus resolve sha256-abc123... mark://hub.example:6309/index.md
```

## Atomic Graph Publishing

With `-publish-graph`, the crawler publishes an atomic snapshot at `/graph/manifest.md` before updating the compatible `/graph.md` export. Snapshot shards live under `/graph/shards/a/` or `/graph/shards/b/`; the manifest pins each shard version and becomes visible only after every shard verifies successfully.

Snapshot-aware clients and brokers prefer the manifest and fall back to `/graph.md` when it is absent. Retention applies to the manifest and legacy export only. Version-pinned shards remain unpruned until reference-aware garbage collection can prove they are unreachable.

## Authentication

The crawler uses the same token resolution as other clients:

1. **`DEMARKUS_AUTH` env var** — fallback for all servers
2. **Stored tokens** — `~/.mark/tokens.toml` per-host tokens

Store tokens before crawling:

```bash
# Read access to private servers being crawled
demarkus token add mark://private.example:6309 <read-token>

# Publish access to hubs (required for -publish flag)
demarkus token add mark://my-hub.example:6309 <publish-token>
```

The crawler automatically uses stored tokens for:
- **Reads** (FETCH, LIST) on private servers being crawled
- **Writes** (PUBLISH) to hubs when using `-publish`

The agent follows LIST continuation cursors until `complete: true`. A server
without completeness metadata, a cursor failure, or a traversal budget limit
makes that crawl incomplete rather than silently publishing authoritative absence.
Deploy LIST-pagination-capable world servers before upgrading the agent; the
agent refuses index and graph publication while any crawled world lacks the
completeness metadata.

### Token Requirements

| Operation | Token Required | Capability |
|-----------|----------------|------------|
| Crawl public servers | No | — |
| Crawl private servers | Yes | `read` |
| Publish to hub | Yes | `publish` |

### Hub Tokens

When using `-publish`, ensure you have `publish` capability tokens stored for each hub in your config:

```toml
hubs = ["mark://hub1.example:6309", "mark://hub2.example:6309"]
```

```bash
# Store publish tokens for each hub
demarkus token add mark://hub1.example:6309 <hub1-publish-token>
demarkus token add mark://hub2.example:6309 <hub2-publish-token>
```

Without hub tokens, the crawler will still complete but index publishing will fail with unauthorized errors.

## Examples

### Crawl a Hub and Index

```bash
# Crawl the demarkus hub and publish to your own hub
demarkus-agent crawl \
  -seeds "mark://hub.demarkus.io" \
  -config my-hub.toml \
  -publish \
  -insecure \
  -v
```

### Development Crawl

```bash
# Quick test against local server
demarkus-agent crawl -seeds "mark://localhost:6309" -insecure -v
```

Output:

```
servers=1 docs=5 hashes=5 errors=0
```

### Production Daemon

```toml
# /etc/demarkus/fedcrawl.toml
seeds = ["mark://hub.demarkus.io", "mark://soul.demarkus.io"]
hubs = ["mark://my-hub.example:6309"]

[crawl]
max_servers = 100
max_documents = 5000
workers = 10

[politeness]
request_delay = "200ms"

[schedule]
interval = "12h"
```

Run as a systemd service (see deployment docs) or directly:

```bash
demarkus-agent daemon -config /etc/demarkus/fedcrawl.toml -publish
```

## Related

- [Tools Overview](../tools/index.md)
- [Run a Server](../server/index.md)
- [Deployment](../deployment/index.md)
