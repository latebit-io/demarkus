# Deployment & TLS

This section covers production deployment of Demarkus with real TLS certificates, firewall configuration, and systemd service management.

## Overview

For production you should:

1. Obtain a trusted TLS certificate (Let's Encrypt or your own).
2. Run the server with `-tls-cert` and `-tls-key`.
3. Open UDP port `6309`.
4. Set up auto-renewal with zero downtime (if using Let's Encrypt).

## TLS with Custom Certificates

If you already have TLS certificates (from your CA, Cloudflare Origin, etc.), you can provide them directly.

### Via Install Script

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | bash -s -- --tls-cert /path/to/cert.pem --tls-key /path/to/key.pem
```

The script copies the certificates into the config directory (`/etc/demarkus/tls/` on Linux, `~/.demarkus/tls/` on macOS) and configures the service to use them.

### Manual Setup

```bash
demarkus-server \
  -root /srv/site \
  -tls-cert /path/to/cert.pem \
  -tls-key /path/to/key.pem
```

To update certificates later, replace the files and send `SIGHUP` to reload without downtime:

```bash
kill -HUP $(pidof demarkus-server)
```

## TLS with Let's Encrypt

### 1) Install Certbot

```bash
sudo apt install certbot
```

### 2) Obtain a Certificate

```bash
sudo certbot certonly --standalone -d yourdomain.com
```

Certificates are saved to:

```
/etc/letsencrypt/live/yourdomain.com/
```

### 3) Start the Server with TLS

```bash
demarkus-server \
  -root /srv/site \
  -tls-cert /etc/letsencrypt/live/yourdomain.com/fullchain.pem \
  -tls-key /etc/letsencrypt/live/yourdomain.com/privkey.pem
```

Or using environment variables:

```bash
export DEMARKUS_ROOT=/srv/site
export DEMARKUS_TLS_CERT=/etc/letsencrypt/live/yourdomain.com/fullchain.pem
export DEMARKUS_TLS_KEY=/etc/letsencrypt/live/yourdomain.com/privkey.pem
demarkus-server
```

## Firewall (UDP 6309)

```bash
sudo ufw allow 6309/udp
```

## Auto-Renew Certificates

Zero‑downtime reloads can be done with `SIGHUP`:

```bash
0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"
```

If you prefer a full restart:

```bash
0 */12 * * * certbot renew --quiet --deploy-hook "systemctl restart demarkus"
```

## Systemd Service

Create `/etc/systemd/system/demarkus.service`:

```ini
[Unit]
Description=Demarkus Mark Protocol Server
After=network.target

[Service]
Type=simple
User=demarkus
ExecStart=/usr/local/bin/demarkus-server
Environment=DEMARKUS_ROOT=/srv/site
Environment=DEMARKUS_TLS_CERT=/etc/letsencrypt/live/yourdomain.com/fullchain.pem
Environment=DEMARKUS_TLS_KEY=/etc/letsencrypt/live/yourdomain.com/privkey.pem
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl enable --now demarkus
```

## Validation

From a client:

```bash
demarkus mark://yourdomain.com/index.md
```

You should see a successful response with status `ok`.

## Related

- [Run a Server](../server/index.md)
- [Configuration Reference](../reference/index.md)
- [Install & Build](../install/index.md)