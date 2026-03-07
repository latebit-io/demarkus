---
layout: default
title: Install on Linux
permalink: /install/linux/
---

# Install on Linux

## Quick install (server + client)

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

> Run with `sudo` so the installer can write to `/usr/local/bin` and install the systemd service.

This installs `demarkus-server`, `demarkus-token`, `demarkus`, `demarkus-tui`, and `demarkus-mcp`.

## With Let's Encrypt TLS

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain yourdomain.com --root /srv/site
```

The installer:
1. Runs `certbot certonly --standalone` to obtain the certificate
2. Configures the server to use it
3. Sets up a cron job for auto-renewal with zero-downtime reload (`SIGHUP`)

**Prerequisite:** Port 80 must be open for the Let's Encrypt HTTP challenge.

## With your own TLS certificate

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --tls-cert /path/to/cert.pem --tls-key /path/to/key.pem
```

## Client only

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --client-only
```

No sudo needed for client-only. Installs to `~/.local/bin` if `/usr/local/bin` is not writable.

## What the installer does

- Downloads binaries for `linux/amd64`, `linux/arm64`, or `linux/arm` from GitHub Releases
- Verifies checksums before installing
- Creates a systemd service at `/etc/systemd/system/demarkus.service`
- Enables and starts the service

## Firewall

Open UDP port 6309:

```bash
sudo ufw allow 6309/udp
```

## Managing the service

```bash
sudo systemctl status demarkus
sudo systemctl restart demarkus
sudo systemctl stop demarkus
journalctl -u demarkus -f
```

## Upgrade

```bash
sudo demarkus-install update
```

Or re-run the install script — it preserves tokens, TLS certs, and content directory.

## Uninstall

```bash
sudo demarkus-install uninstall
```

## Build from source

```bash
git clone https://github.com/latebit-io/demarkus.git
cd demarkus
make all
```

Requires Go 1.22+.

## Related

- [Getting Started](/getting-started/)
- [Public hub scenario](/scenarios/public-hub/)
- [Deployment & TLS](/setup/) (full reference)
- [Troubleshooting](/troubleshooting/)
