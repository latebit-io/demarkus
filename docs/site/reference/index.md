# Reference

This section is a stable reference for configuration, environment variables, and protocol documentation. Use it as a lookup while running or deploying a Demarkus server.

## Configuration

- [Server configuration & env vars](#server-configuration)

### Server configuration

All settings are via environment variables; flags override for dev use:

| Env var | Flag | Default | Description |
|---------|------|---------|-------------|
| `DEMARKUS_ROOT` | `-root` | *(required)* | Content directory to serve |
| `DEMARKUS_PORT` | `-port` | `6309` | UDP port to listen on |
| `DEMARKUS_TLS_CERT` | `-tls-cert` | *(dev cert)* | Path to TLS certificate PEM |
| `DEMARKUS_TLS_KEY` | `-tls-key` | *(dev cert)* | Path to TLS private key PEM |
| `DEMARKUS_TOKENS` | `-tokens` | *(none — writes disabled)* | Path to TOML tokens file |
| `DEMARKUS_MAX_STREAMS` | — | `10` | Max concurrent streams per connection |
| `DEMARKUS_IDLE_TIMEOUT` | — | `30s` | Idle connection timeout |
| `DEMARKUS_REQUEST_TIMEOUT` | — | `10s` | Per-request deadline |

Notes:
- `-tls-cert` and `-tls-key` must be provided together.
- When no tokens file is configured, the server is read-only.

## Protocol

- [Protocol Specification](../../spec.md)

## Related

- [Install & Build](../install/index.md)
- [Run a Server](../server/index.md)
- [Use the Clients](../client/index.md)
- [Deployment & TLS](../deployment/index.md)
- [Tools](../tools/index.md)