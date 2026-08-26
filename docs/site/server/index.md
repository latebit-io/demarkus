# Run a Server

This section covers how to configure, start, and operate a Demarkus server. It includes secure-by-default behavior, authentication, and health checks.

## Overview

A Demarkus server serves markdown files over QUIC. It is **read-only by default** and requires a tokens file to allow `PUBLISH`.

## Quick Start (Dev)

```bash
./server/bin/demarkus-server -root ./docs/site
```

This uses a self-signed development certificate and listens on UDP port `6309`.

## Minimum Configuration

The only required setting is the content directory:

```bash
demarkus-server -root /srv/site
```

## Authentication

Writes are denied unless you provide a tokens file. Reads are public by default.

### Write Access

#### 1) Generate a token

```bash
./tools/bin/demarkus-token generate -paths "/**" -ops publish -tokens /etc/demarkus/tokens.toml
```

#### 2) Start the server with tokens

```bash
demarkus-server -root /srv/site -tokens /etc/demarkus/tokens.toml
```

#### 3) Publish with the token

```bash
demarkus --insecure -X PUBLISH -auth <raw-token> -body "# Hello World" mark://localhost:6309/hello.md
```

### Read Access (Private Paths)

By default, all paths are public. To protect specific paths, create a token with the `read` operation. Any path covered by a read token requires authentication for FETCH, LIST, and VERSIONS. LOOKUP doesn't reject the whole request; instead it filters its results, omitting any protected document the caller isn't authorized to read (no path, title, or existence is revealed).

#### Protect a subtree

```bash
./tools/bin/demarkus-token generate -paths "/internal/**" -ops read -tokens /etc/demarkus/tokens.toml
```

Now `/internal/**` requires a read token. Everything else stays public.

#### Full private server

```bash
./tools/bin/demarkus-token generate -paths "/**" -ops "read,publish" -tokens /etc/demarkus/tokens.toml
```

This protects all paths. The well-known manifest (`/.well-known/agent-manifest.md`) is always public.

#### Accessing private paths from clients

Store the read token on the client, then all three clients (CLI, TUI, MCP) use it automatically:

```bash
# Store once
demarkus token add mark://private.example:6309 <raw-token>

# CLI: token sent automatically
demarkus mark://private.example:6309/internal/doc.md

# TUI: token sent automatically
demarkus-tui mark://private.example:6309/internal/doc.md

# MCP: uses stored token, or pass -token flag
demarkus-mcp -host mark://private.example:6309
```

See [Use the Clients](../client/index.md#accessing-private-servers) for full details.

## Read-Only Mode

For public-facing servers where you don't want any network writes, use the `-read-only` flag:

```bash
demarkus-server -read-only -root /srv/site
```

All PUBLISH, APPEND, and ARCHIVE requests are rejected with `not-permitted`. FETCH, LIST, VERSIONS, and LOOKUP work normally.

Publish content locally with `demarkus-publish`; it writes directly to the versioned store on disk:

```bash
demarkus-publish -root /srv/site -path /index.md -body "# Hello"
echo "# Hello" | demarkus-publish -root /srv/site -path /index.md
```

For maximum security, combine with the [read-only chroot install](../security/index.md#read-only-mode-maximum-lockdown). See also [Security Model](../security/index.md).

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

## Multi-World Mode (`demarkus-knowledge-server`)

`demarkus-knowledge-server` is the production server: one process hosts many logically isolated worlds on one UDP listener, with TLS SNI selecting the world during the QUIC handshake. Each world is backed by its own GCS bucket, and any number of stateless replicas can share the buckets.

It takes one required flag, `-config`, pointing at a strict YAML file:

```yaml
version: 1
listen:
  address: ":6309"
health:
  address: ":8081"
tls:
  certFile: /run/demarkus/tls/tls.crt
  keyFile: /run/demarkus/tls/tls.key
worlds:
  - name: acme
    authorities: ["acme.knowledge.example.com"]
    bucket:
      url: gs://acme-world
      worldID: <uuid>
    auth:
      tokensFile: /run/demarkus/worlds/acme/tokens.toml
    policy:
      path: /.well-known/demarkus/policy.md
```

Operational notes:

- `/livez` and `/readyz` are served as plain HTTP on the health address.
- `SIGHUP` reloads TLS certificates and token files; token files also hot-reload on change.
- GCS credentials come from Application Default Credentials (Workload Identity on GKE); there are no credential flags or env vars.
- Each world's bucket must be initialized with `demarkus-knowledge-bootstrap` before the server can open it.

Deploy it with the Helm chart; see [Kubernetes & Helm](../deployment/kubernetes.md) and `deploy/helm/demarkus-knowledge-server/README.md`.

## Next Steps

- [Deploy with TLS](../deployment/index.md)
- [Use the Clients](../client/index.md)
- [Configuration Reference](../reference/index.md)

## Related

- [Install & Build](../install/index.md)
- [Install Script](../install/index.md#install-script)
- [Protocol Spec](../../SPEC.md)