---
layout: default
title: Install on macOS
permalink: /install/macos/
---

# Install on macOS

## Client only (recommended first step)

Install the CLI, TUI browser, and MCP server with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

This installs to `~/.local/bin`. Add it to your PATH if needed:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

Verify:

```bash
demarkus --help
demarkus-tui --help
demarkus-mcp --help
```

## Server + client (full install)

### With your own TLS certificate

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --tls-cert /path/to/cert.pem --tls-key /path/to/key.pem
```

### Without TLS (dev/local only)

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

This uses a built-in self-signed certificate. Use `--insecure` on the client when connecting.

### What the installer does

- Downloads binaries for `darwin/arm64` or `darwin/amd64` from GitHub Releases
- Verifies checksums before installing
- Creates a launchd plist at `~/Library/LaunchAgents/io.demarkus.server.plist`
- Starts the server automatically on login
- Copies TLS certificates to `~/.demarkus/tls/` if provided

## Running manually (without launchd)

```bash
demarkus-server -root ~/my-docs
```

With TLS:

```bash
demarkus-server -root ~/my-docs -tls-cert ~/cert.pem -tls-key ~/key.pem
```

## Build from source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all
```

Binaries are placed in `server/bin/` and `client/bin/`. Requires Go 1.22+.

## Upgrade

```bash
demarkus-install update
```

Or re-run the install script — it preserves your tokens, TLS certs, and content directory.

## Uninstall

```bash
demarkus-install uninstall
```

## Related

- [Getting Started](/getting-started/)
- [Personal knowledge base scenario](/scenarios/personal-wiki/)
- [Troubleshooting](/troubleshooting/)
