# Reference

This section is a stable reference for configuration, environment variables, and protocol documentation. Use it as a lookup while running or deploying a Demarkus server.

## Configuration

- [Server configuration & env vars](#server-configuration)
- [Federation crawler configuration](#federation-crawler)

### Server configuration

All settings are via environment variables; flags override for dev use:

| Env var | Flag | Default | Description |
|---------|------|---------|-------------|
| `DEMARKUS_ROOT` | `-root` | *(required)* | Content directory to serve |
| `DEMARKUS_PORT` | `-port` | `6309` | UDP port to listen on |
| `DEMARKUS_TLS_CERT` | `-tls-cert` | *(dev cert)* | Path to TLS certificate PEM |
| `DEMARKUS_TLS_KEY` | `-tls-key` | *(dev cert)* | Path to TLS private key PEM |
| `DEMARKUS_TOKENS` | `-tokens` | *(none — writes disabled)* | Path to TOML tokens file |
| `DEMARKUS_READ_ONLY` | `-read-only` | *(disabled)* | Reject all write operations (`1`, `true`, or `yes`) |
| `DEMARKUS_MAX_STREAMS` | — | `10` | Max concurrent streams per connection |
| `DEMARKUS_IDLE_TIMEOUT` | — | `30s` | Idle connection timeout |
| `DEMARKUS_REQUEST_TIMEOUT` | — | `10s` | Per-request deadline |

Notes:
- `-tls-cert` and `-tls-key` must be provided together.
- When no tokens file is configured, the server rejects writes (no auth = no writes).
- `-read-only` explicitly rejects all write operations regardless of tokens. Use this for public-facing servers where content is published locally with `demarkus-publish`.

### Federation crawler configuration

The federation crawler (`demarkus-agent`) uses a TOML configuration file:

```toml
seeds = ["mark://server1:6309", "mark://server2:6309"]
hubs = ["mark://hub.example:6309"]

[endpoints."server1:6309"]
dial_address = "server1.internal:6309"
server_name = "server1.internal"

[crawl]
max_servers = 50      # Maximum servers to discover
max_documents = 1000  # Maximum documents per crawl run
workers = 5           # Concurrent fetch goroutines

[politeness]
request_delay = "100ms"  # Delay between requests to same server

[schedule]
interval = "6h"  # Time between crawl runs (daemon mode)
```

#### Config fields

| Field | Default | Description |
|-------|---------|-------------|
| `seeds` | *(required)* | Initial servers to crawl |
| `hubs` | *(empty)* | Servers to publish hash indexes to |
| `endpoints.<authority>.dial_address` | *(authority)* | Socket destination override |
| `endpoints.<authority>.server_name` | *(authority host)* | TLS SNI and certificate name override |
| `crawl.max_servers` | `50` | Maximum servers to discover per run |
| `crawl.max_documents` | `1000` | Maximum documents to fetch per run |
| `crawl.workers` | `5` | Concurrent fetch goroutines |
| `politeness.request_delay` | `100ms` | Delay between requests to same server |
| `schedule.interval` | — | Time between runs (daemon mode, required) |

#### CLI flags

Flags override config file values:

| Flag | Description |
|------|-------------|
| `-config <path>` | Path to TOML config file |
| `-seeds <urls>` | Comma-separated seed servers (overrides config) |
| `-state <path>` | State file path (default `~/.mark/fedcrawl.json`) |
| `-insecure` | Skip TLS certificate verification |
| `-publish` | Publish indexes to hubs after crawl |
| `-per-server` | Publish per-server indexes (not aggregated) |
| `-v` | Verbose output |

#### Environment variables

The crawler uses the same token resolution as other clients:

| Env var | Description |
|---------|-------------|
| `DEMARKUS_AUTH` | Fallback auth token for all servers |

For per-server tokens, use `demarkus token add mark://host:6309 <token>` before crawling.

## Authoring

- [Supported Markdown Features](markdown.md) — what the TUI renders and what the link graph tracks
- [Open Knowledge Format compatibility](markdown.md#open-knowledge-format-okf-compatibility) — OKF field mapping, default `type`, and the `demarkus okf` validate/import/export codec

## Protocol

- [Protocol Specification](../../SPEC.md)
- [Agent Manifest](agent-manifest.md) — discovery convention for AI agents

## Related

- [Install & Build](../install/index.md)
- [Run a Server](../server/index.md)
- [Use the Clients](../client/index.md)
- [Deployment & TLS](../deployment/index.md)
- [Tools](../tools/index.md)
