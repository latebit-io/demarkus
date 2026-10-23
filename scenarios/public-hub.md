---
layout: default
title: Public Server
permalink: /scenarios/public-hub/
---

# Public server

Publish docs, specs, or a knowledge base on the open web of `mark://`: a VPS,
a real certificate, open reads, token-gated writes.

## You need

- A Linux VPS (Ubuntu 22.04+)
- A domain pointing at it
- Port 80/TCP (Let's Encrypt) and 6309/UDP open

## Set up

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  sudo bash -s -- --domain yourdomain.com --root /srv/site

sudo ufw allow 6309/udp
sudo ufw allow 80/tcp
```

The installer fetches the certificate, starts the systemd service, and renews
with a zero-downtime reload (`SIGHUP`). Add `--with-library` for a reading room
in the browser.

Check from anywhere:

```bash
demarkus mark://yourdomain.com/health
```

## Publish

**On the server**, straight to the store. The server can then run
[read-only](/security/#read-only-mode-maximum-lockdown):

```bash
demarkus-publish -root /srv/site -path /index.md -body "# Welcome"
```

**Remotely**, with a token:

```bash
sudo demarkus-token generate -label writer -paths "/**" -ops publish -tokens /etc/demarkus/tokens.toml
sudo systemctl restart demarkus

demarkus -X PUBLISH -auth "$TOKEN" -expected-version 0 \
  -body "# A new page" mark://yourdomain.com/new-page.md
```

`-expected-version 0` creates; pass the fetched version to replace, or use
`demarkus edit`.

## Be a hub

A hub is a server whose documents link to other servers: a curated directory.
Hubs can link to hubs. Publish `/.well-known/agent-manifest.md` (purpose, key
paths, how to write) and agents find their way with `mark_discover`.

Agents can also crawl a set of servers with `mark_graph` and publish what they
found with `mark_graph_publish` (full tool profile). The result is plain
markdown with `mark://` links that the next agent can follow.
[The graph →](/graph/)

## Operate

```bash
sudo systemctl status demarkus
journalctl -u demarkus -f
```

[Install on Linux](/install/linux/) · [Security](/security/) · [Troubleshooting](/troubleshooting/)
