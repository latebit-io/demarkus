# Install & Build

This section covers how to install Demarkus from source or pre-built binaries. If you're new, start here.

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
| `demarkus-token` | `server/bin/demarkus-token` | Generate auth tokens |
| `demarkus` | `client/bin/demarkus` | CLI (fetch/list/publish) |
| `demarkus-tui` | `client/bin/demarkus-tui` | TUI browser |
| `demarkus-mcp` | `client/bin/demarkus-mcp` | MCP server |

> Build note: use `make server` / `make client` or `go build -o bin/<name> ./cmd/<name>/` to avoid dropping binaries in the working directory.

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
```

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

## Next Steps

- [Run a Server](../server/index.md)
- [Use the Clients](../client/index.md)