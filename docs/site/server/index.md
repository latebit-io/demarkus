# Run a Server

This section covers how to configure, start, and operate a Demarkus server. It includes secure-by-default behavior, authentication, and health checks.

## Overview

A Demarkus server serves markdown files over QUIC. It is **read-only by default** and requires a tokens file to allow `PUBLISH`.

## Quick Start (Dev)

```bash
./server/bin/demarkus-server -root ./examples/demo-site
```

This uses a self-signed development certificate and listens on UDP port `6309`.

## Minimum Configuration

The only required setting is the content directory:

```bash
demarkus-server -root /srv/site
```

## Authentication (Write Access)

Writes are denied unless you provide a tokens file.

### 1) Generate a token

```bash
./server/bin/demarkus-token generate -paths "/*" -ops publish -tokens /etc/demarkus/tokens.toml
```

### 2) Start the server with tokens

```bash
demarkus-server -root /srv/site -tokens /etc/demarkus/tokens.toml
```

### 3) Publish with the token

```bash
demarkus --insecure -X PUBLISH -auth <raw-token> mark://localhost:6309/hello.md -body "# Hello World"
```

## Health Check

The server exposes a lightweight health endpoint:

```bash
demarkus --insecure mark://localhost:6309/health
```

A healthy server returns:

```
[ok]
```

## Logs & Behavior

- Logs requests as: `[REQUEST] VERB /path`
- Denies writes when no tokens file is set
- Enforces path traversal protection
- Limits file size to 1 MB

## Next Steps

- [Deploy with TLS](../deployment/index.md)
- [Use the Clients](../client/index.md)
- [Configuration Reference](../reference/index.md)

## Related

- [Install & Build](../install/index.md)
- [Install Script](../install/index.md#install-script)
- [Protocol Spec](../../spec.md)