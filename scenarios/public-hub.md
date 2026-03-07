---
layout: default
title: Public Hub
permalink: /scenarios/public-hub/
---

# Public Hub

Run a publicly accessible Demarkus server with TLS, open read access, and optional write tokens. Good for publishing docs, specs, or knowledge bases.

## What you'll have

- A server on a VPS with a real domain and Let's Encrypt certificate
- Open read access over `mark://` (no auth required to read)
- Write access restricted to token holders
- Auto-renewing TLS with zero-downtime reloads

## Prerequisites

- A Linux VPS (Ubuntu 22.04+ recommended)
- A domain name pointing to the VPS (A record → VPS IP)
- Port 80 open (for Let's Encrypt HTTP challenge)
- Port 6309/UDP open (for Mark Protocol)

## Setup

### 1. Install with Let's Encrypt

SSH into your VPS and run:

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain yourdomain.com --root /srv/site
```

This will:
- Install all binaries to `/usr/local/bin`
- Obtain a Let's Encrypt certificate via certbot
- Configure and start the systemd service
- Set up auto-renewal with zero-downtime reload

### 2. Open firewall

```bash
sudo ufw allow 6309/udp
sudo ufw allow 80/tcp    # for Let's Encrypt renewal
```

### 3. Add your initial content

```bash
sudo mkdir -p /srv/site
echo "# Welcome\n\nThis is a public Demarkus hub." | sudo tee /srv/site/index.md
```

### 4. Verify

From your local machine:

```bash
demarkus mark://yourdomain.com/index.md
```

You should see the document with `status: ok`.

### 5. Enable writes (optional)

Generate a publish token on the server:

```bash
sudo demarkus-token generate -paths "/*" -ops publish -tokens /etc/demarkus/tokens.toml
```

Then reload the server config:

```bash
sudo systemctl restart demarkus
```

Publish from your local machine:

```bash
demarkus -X PUBLISH -auth <your-token> mark://yourdomain.com/hello.md -body "# Hello"
```

## Certificate renewal

The installer configures this cron job automatically:

```bash
0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"
```

The `SIGHUP` signal triggers a zero-downtime certificate reload — no connection drops.

## Monitoring

```bash
sudo systemctl status demarkus
journalctl -u demarkus -f
```

Health check:

```bash
demarkus mark://yourdomain.com/health
```

## Related

- [Install on Linux](/install/linux/)
- [Getting Started](/getting-started/)
- [Troubleshooting](/troubleshooting/)
