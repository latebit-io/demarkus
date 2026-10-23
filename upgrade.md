---
layout: default
title: Upgrade & Uninstall
permalink: /upgrade/
nav: docs
---

# Upgrade & Uninstall

## Plugins

Update through your agent's plugin manager. Each plugin release pins its
binaries; the next session installs them into `~/.demarkus/bin`. Your memory is
untouched.

## Knowledge systems

Upgrade through Helm or your GitOps configuration.

## Binaries

### Using demarkus-install (recommended)

If you installed via the install script, use the built-in update command:

```bash
# macOS (no sudo needed; installs to ~/.local/bin or /usr/local/bin based on original install)
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
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | sudo bash
```

## What gets updated

- `demarkus-server` (server release), `demarkus-token` and `demarkus-publish` (tools release)
- `demarkus`, `demarkus-tui`, `demarkus-mcp` (client releases)
- `demarkus-library`, when installed via `--with-library`
- `demarkus-install` (the update helper itself)

## What is preserved

- `/etc/demarkus/tokens.toml` (or `~/.demarkus/tokens.toml` on macOS)
- TLS certificates in `/etc/demarkus/tls/` (or `~/.demarkus/tls/`)
- Your content directory (untouched)
- Service configuration (plist or systemd unit): only rewritten if flags change

## Check installed version

`demarkus-server -version` and `demarkus-mcp -version` print their releases.
The installer also records the installed release in a version marker:

```bash
# Linux
cat /etc/demarkus/version

# macOS
cat ~/.demarkus/version
```

`demarkus-install update` reads this same marker and reports the current vs.
latest release before updating.

## Uninstall

```bash
# macOS
demarkus-install uninstall

# Linux
sudo demarkus-install uninstall
```

This removes the binaries, the service, and the installer's config: tokens,
TLS certificates, the version marker. You are asked before the content
directory is removed; `--keep-data` keeps it without asking.

- **Linux**: config is `/etc/demarkus`, removed whole.
- **macOS**: `~/.demarkus` is shared with the memory plugins. Only the installer's own files go; the plugins' binaries, state, and soul stay.

Installed before October 23, 2026? Run `demarkus-install update` first: it
fetches the current helper, whose macOS uninstall keeps the plugins' files.

## Related

- [Install on macOS](/install/macos/)
- [Install on Linux](/install/linux/)
- [Troubleshooting](/troubleshooting/)
