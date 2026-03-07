---
layout: default
title: Upgrade & Uninstall
permalink: /upgrade/
---

# Upgrade & Uninstall

## Upgrade

### Using demarkus-install (recommended)

If you installed via the install script, use the built-in update command:

```bash
# macOS (no sudo needed — installs to ~/.local/bin or /usr/local/bin based on original install)
demarkus-install update

# Linux server install (sudo required if installed to /usr/local/bin)
sudo demarkus-install update
```

This downloads the latest server and client releases, verifies checksums, replaces binaries, and restarts the service.

### Re-running the install script

Re-running the install script is safe and idempotent. It:

- Preserves your tokens file
- Preserves your TLS certificates
- Does not touch your content directory
- Stops and restarts the service cleanly

```bash
# macOS client update
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only

# Linux server + client update
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

## What gets updated

- `demarkus-server`, `demarkus-token` (server releases)
- `demarkus`, `demarkus-tui`, `demarkus-mcp` (client releases)
- `demarkus-install` (the update helper itself)

## What is preserved

- `/etc/demarkus/tokens.toml` (or `~/.demarkus/tokens.toml` on macOS)
- TLS certificates in `/etc/demarkus/tls/` (or `~/.demarkus/tls/`)
- Your content directory (untouched)
- Service configuration (plist or systemd unit) — only rewritten if flags change

## Check installed versions

```bash
demarkus --version
demarkus-server --version
```

## Uninstall

```bash
# macOS
demarkus-install uninstall

# Linux
sudo demarkus-install uninstall
```

This removes all installed binaries and the service configuration. Your content directory and tokens file are **not** removed.

To also remove config and data:

```bash
# macOS
rm -rf ~/.demarkus

# Linux
sudo rm -rf /etc/demarkus
```

## Related

- [Install on macOS](/install/macos/)
- [Install on Linux](/install/linux/)
- [Troubleshooting](/troubleshooting/)
