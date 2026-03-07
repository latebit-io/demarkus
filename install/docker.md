---
layout: default
title: Install with Docker
permalink: /install/docker/
---

# Install with Docker

Multi-arch images are published to GitHub Container Registry on every release: `linux/amd64`, `linux/arm64`, and `linux/arm/v7`.

```
ghcr.io/latebit-io/demarkus-server:latest
```

## Quick start

```bash
docker run -d \
  --name demarkus \
  -p 6309:6309/udp \
  -v /srv/site:/data \
  ghcr.io/latebit-io/demarkus-server:latest \
  -root /data
```

Fetch a document to verify:

```bash
demarkus --insecure mark://localhost:6309/index.md
```

> Use `--insecure` because the container uses the built-in self-signed certificate by default.

## With TLS

Mount your certificates and pass the paths:

```bash
docker run -d \
  --name demarkus \
  -p 6309:6309/udp \
  -v /srv/site:/data \
  -v /etc/certs:/certs:ro \
  ghcr.io/latebit-io/demarkus-server:latest \
  -root /data \
  -tls-cert /certs/fullchain.pem \
  -tls-key /certs/privkey.pem
```

## With write tokens

Generate a tokens file on the host, then mount it:

```bash
demarkus-token generate -paths "/*" -ops publish -tokens /srv/site/tokens.toml
```

```bash
docker run -d \
  --name demarkus \
  -p 6309:6309/udp \
  -v /srv/site:/data \
  ghcr.io/latebit-io/demarkus-server:latest \
  -root /data \
  -tokens /data/tokens.toml
```

## docker-compose

```yaml
services:
  demarkus:
    image: ghcr.io/latebit-io/demarkus-server:latest
    restart: unless-stopped
    ports:
      - "6309:6309/udp"
    volumes:
      - /srv/site:/data
      - /etc/letsencrypt/live/yourdomain.com:/certs:ro
    command:
      - -root
      - /data
      - -tls-cert
      - /certs/fullchain.pem
      - -tls-key
      - /certs/privkey.pem
      - -tokens
      - /data/tokens.toml
```

Start:

```bash
docker compose up -d
```

## Certificate renewal (Let's Encrypt)

The server reloads certificates on `SIGHUP` without downtime:

```bash
# In your certbot deploy hook:
docker kill --signal=HUP demarkus
```

Or with docker-compose:

```bash
docker compose kill -s HUP demarkus
```

## Upgrading

```bash
docker pull ghcr.io/latebit-io/demarkus-server:latest
docker compose up -d
```

## Notes

- The image is built `FROM scratch` — no shell, no OS, just the binary. Keep this in mind for debugging.
- Content directory and tokens file live on the host via volume mounts — they persist across container restarts and upgrades.
- The image exposes UDP port 6309 only. No HTTP, no healthcheck endpoint over TCP.

## Related

- [Install on Linux](/install/linux/)
- [Public hub scenario](/scenarios/public-hub/)
- [Troubleshooting](/troubleshooting/)
