# Reference

This section is a stable reference for configuration, environment variables, and protocol documentation. Use it as a lookup while running or deploying a Demarkus server.

## Configuration

- [Server configuration & env vars](#server-configuration)
- [Knowledge server configuration](#knowledge-server-configuration)

Broker configuration (OIDC, worlds, the MCP gateways) is documented in `knowledge/cmd/demarkus-knowledge/` and the `demarkus-knowledge-server` Helm chart README.

### Server configuration

All settings are via environment variables; flags override for dev use:

| Env var | Flag | Default | Description |
|---------|------|---------|-------------|
| `DEMARKUS_ROOT` | `-root` | *(required)* | Content directory to serve |
| `DEMARKUS_PORT` | `-port` | `6309` | UDP port to listen on |
| `DEMARKUS_TLS_CERT` | `-tls-cert` | *(dev cert)* | Path to TLS certificate PEM |
| `DEMARKUS_TLS_KEY` | `-tls-key` | *(dev cert)* | Path to TLS private key PEM |
| `DEMARKUS_TOKENS` | `-tokens` | *(none; writes disabled)* | Path to TOML tokens file |
| `DEMARKUS_READ_ONLY` | `-read-only` | *(disabled)* | Reject all write operations (`1`, `true`, or `yes`) |
| `DEMARKUS_MAX_STREAMS` | - | `10` | Max concurrent streams per connection |
| `DEMARKUS_MAX_WATCHES` | - | `1024` | Open WATCH streams per server; each connection may hold half its streams as watches |
| `DEMARKUS_IDLE_TIMEOUT` | - | `30s` | Idle connection timeout |
| `DEMARKUS_REQUEST_TIMEOUT` | - | `10s` | Per-request deadline |
| `DEMARKUS_LOG_FORMAT` | - | `text` | Log output format (`text` or `json`) |

Notes:
- `-tls-cert` and `-tls-key` must be provided together.
- When no tokens file is configured, the server rejects writes (no auth = no writes).
- `-read-only` explicitly rejects all write operations regardless of tokens. Use this for public-facing servers where content is published locally with `demarkus-publish`.

### Knowledge server configuration

`demarkus-knowledge` reads no environment variables: all configuration is a strict YAML file passed with `-server-config`, and GCS credentials come from Application Default Credentials (Workload Identity on GKE). Unknown keys are rejected, and durations must be quoted strings.

```yaml
version: 1                      # required, only supported value
listen:
  address: ":6309"              # default
  maxIncomingStreams: 128       # default
  idleTimeout: "30s"            # default
health:
  address: ":8081"              # default; serves /livez and /readyz
peers:                          # replicas hint each other about commits; needed by more than one replica
  listen: ":6310"               # replica-only hint listener (ALPN mark-peer); empty disables
  service: <dns-name>           # headless Service resolved to the peers, this host left out
  addresses: [<host:port>]      # or fixed peers, for deployments without DNS
tls:
  certFile: <path>              # required
  keyFile: <path>               # required
worlds:                         # one or more
  - name: <dns-label>           # unique
    authorities: [<dns-name>]   # one or more, globally unique
    bucket:
      url: gs://<bucket>        # exact form
      worldID: <uuid>           # canonical lowercase RFC 4122, unique
      checkpointGrace: "15m"    # default and minimum; how long superseded checkpoints stay
    # dir: <absolute path>      # instead of bucket: a file world, one replica (no peers), unique, not nested
    auth:
      tokensFile: <path>        # required; the broker appends to it
      staticTokensFile: <path>  # optional; operator-owned entries merged in
    policy:
      path: /.well-known/demarkus/policy.md   # only supported value
      file: /etc/demarkus/policy-acme.md      # optional: body seeded as version 1
    readOnly: false
    limits:
      maxConcurrentRequests: 32 # default
      maxWatches: 1024 # default; open WATCH streams in the world
      requestTimeout: "10s"     # default
      requestsPerSecond: 50     # default
      burst: 100                # default
```

See [Run a Server](../server/index.md#multi-world-mode-demarkus-knowledge-server) and `deploy/helm/demarkus-knowledge-server/README.md`.

## Vocabulary

- [Glossary](glossary.md): one name per concept (world, memory, knowledge system, hub, promote), with the older terms each replaces

## Authoring

- [Supported Markdown Features](markdown.md): what the TUI renders and what the link graph tracks
- [Open Knowledge Format compatibility](markdown.md#open-knowledge-format-okf-compatibility): OKF field mapping, default `type`, and the `demarkus okf` validate/import/export codec

## Protocol

- [Protocol Specification](../../SPEC.md)
- [Agent Manifest](agent-manifest.md): discovery convention for AI agents

## Related

- [Install & Build](../install/index.md)
- [Run a Server](../server/index.md)
- [Use the Clients](../client/index.md)
- [Deployment & TLS](../deployment/index.md)
- [Tools](../tools/index.md)
