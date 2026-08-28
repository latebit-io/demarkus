# Install & Build

This section covers how to install Demarkus from source or pre-built binaries. If you're new, start here.

## Which install do I want?

Demarkus has four install paths, distinguished by **where it runs and who reaches it**:

| Path | Runs where | Exposure | Use it for |
|---|---|---|---|
| **demarkus-memory plugin** (`/soul-init`) | your laptop | local only, no ports, no TLS | a personal soul for one agent; nothing to operate |
| **`install.sh`** | any Linux (or macOS) host | local by default; public with `--domain` | one server: a LAN/dev world, or a public server with real TLS |
| **`install-stack.sh`** (the appliance) | a **public** Linux VPS | public HTTPS on 80/443 **and the world on UDP 6309** | the full self-hosted knowledge system in one command, plus the world as your personal remote soul (see [the five-minute appliance](../deployment/appliance.md)) |
| **Helm charts** (`deploy/helm/`) | a Kubernetes cluster | cluster-defined | multi-replica production deployments, including the multi-world knowledge server (see [Kubernetes & Helm](../deployment/kubernetes.md)) |

The knowledge server (`demarkus-knowledge-server`) has no install-script path: it deploys via Helm only.

The appliance is **not** a local tool: without `--domain` it serves `library.`, `broker.`, `auth.`, and `soul.<public-ip>.sslip.io` and provisions a Let's Encrypt certificate for each, all of which need a reachable public address. For a single server that also runs fine on a laptop or private network, use `install.sh` without `--domain`. For a personal, zero-exposure soul, use the plugin.

## Options

- [Build from Source](#build-from-source)
- [Pre-built Binaries](#pre-built-binaries)
- [Install Script](#install-script)
- [Verify Installation](#verify-installation)

## Build from Source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all
```

This produces the following binaries:

| Binary | Location | Purpose |
|--------|----------|---------|
| `demarkus-server` | `server/bin/demarkus-server` | Serve documents |
| `demarkus-token` | `tools/bin/demarkus-token` | Generate auth tokens |
| `demarkus` | `client/bin/demarkus` | CLI (fetch/list/publish) |
| `demarkus-tui` | `client/bin/demarkus-tui` | TUI browser |
| `demarkus-mcp` | `client/bin/demarkus-mcp` | MCP server |
| `demarkus-agent` | `client/bin/demarkus-agent` | Federation crawler |
| `demarkus-knowledge-broker` | `tools/bin/demarkus-knowledge-broker` | OIDC broker and MCP gateway |
| `demarkus-publish` | `tools/bin/demarkus-publish` | Local publish (bypasses server) |
| `demarkus-loadtest` | `tools/bin/demarkus-loadtest` | Load testing |

`make knowledge-server` builds the multi-world GCS server and its bucket bootstrapper (`server/bin/demarkus-knowledge-server`, `server/bin/demarkus-knowledge-bootstrap`); it is not part of `make all`.

> Build note: each module builds separately; use `make server` / `make client` / `make tools`, or from inside a module `cd server && go build -o bin/<name> ./cmd/<name>/` (same shape for `client/` and `tools/`).

## Pre-built Binaries

Download from [GitHub Releases](https://github.com/latebit-io/demarkus/releases). The server and client are released independently.

After downloading:

```bash
chmod +x demarkus-server demarkus demarkus-tui demarkus-mcp
```

(Optional) Move them into your `PATH`:

```bash
sudo mv demarkus-server demarkus demarkus-tui demarkus-mcp /usr/local/bin/
```

## Install Script

You can install Demarkus with the official install script:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

Common options:

```bash
# Install server + client with Let's Encrypt TLS
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --domain example.com --root /srv/site

# Install server + client with your own certificates
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --tls-cert /path/cert.pem --tls-key /path/key.pem

# Install client-only
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only

# Install read-only server (chrooted, maximum security)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-readonly.sh | sudo bash -s -- --domain example.com
```

The read-only install runs the server in a chroot with zero write access. Publish content locally with `demarkus-publish`. See [Security Model](../security/index.md#read-only-mode-maximum-lockdown) for details.

For private repositories, provide a GitHub token:

```bash
curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | GITHUB_TOKEN=$GITHUB_TOKEN bash
```

After install:

```bash
demarkus-install update
demarkus-install uninstall
```

## Verify Installation

```bash
demarkus --help
demarkus-server --help
```

If those run successfully, move on to the next step.

## Try It Out

Serve the project docs locally and browse them over the protocol:

```bash
./server/bin/demarkus-server -root ./docs/site
./client/bin/demarkus --insecure mark://localhost:6309/index.md
```

## Next Steps

- [Run a Server](../server/index.md)
- [Use the Clients](../client/index.md)
