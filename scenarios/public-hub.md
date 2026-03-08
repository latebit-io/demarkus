---
layout: default
title: Public Hub
permalink: /scenarios/public-hub/
---

# Public Hub

Run a publicly accessible Demarkus server with TLS, open read access, and optional write tokens. Good for publishing docs, specs, or knowledge bases.

## The Hub Pattern

A **hub** is a demarkus server that acts as a discovery index — it links to other demarkus servers rather than hosting original content. Think of it as a curated directory for the demarkus network.

Hubs organize servers into categories (tools, blogs, projects, other hubs) and publish an agent manifest at `/.well-known/agent-manifest.md` so LLM agents can discover and navigate the network automatically.

The more hubs link to each other, the richer the network becomes. Anyone can run a hub.

### Reference Implementation

The public hub at [`mark://hub.demarkus.io`](https://github.com/latebit-io/demarkus-hub) is a working example. Its content lives in a Git repo and CI publishes to the live server on every push to `main`. Browse it:

```bash
demarkus mark://hub.demarkus.io/index.md
```

See the repo for structure, contributing guidelines, and how to list your own server: [github.com/latebit-io/demarkus-hub](https://github.com/latebit-io/demarkus-hub)

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

## Agent Discovery via Published Graphs

Hubs are natural hosts for published graph documents. An agent crawls a set of servers, then publishes its graph to a hub so other agents can discover the topology without recrawling.

### How it works

1. An agent crawls servers using `mark_graph` to build a local graph store
2. The agent publishes the graph to the hub using `mark_graph_publish` (e.g. `/graphs/my-network.md`)
3. Another agent fetches the published graph and runs `mark_graph` on it
4. The crawler follows all `mark://` links in the document, reconstructing the topology instantly

The published graph is plain markdown with `mark://` links in a table. No special format — the same link extraction the crawler already uses parses it naturally.

### Multi-agent discovery

Multiple agents can publish their graphs to the same hub:

- Agent A publishes its graph from crawling `mark://server-a.com`
- Agent B publishes its graph from crawling `mark://server-b.com`
- Agent C crawls both published graph documents — instantly inheriting both topologies

Each published graph is versioned, so you get a history of how the network evolved over time.

### MCP tools

| Tool | Purpose |
|------|---------|
| `mark_graph` | Crawl links from a document, persist to local graph store |
| `mark_graph_export` | Export the local graph as publishable markdown |
| `mark_graph_publish` | Export and publish the graph to a server in one step |
| `mark_backlinks` | Query the local graph for reverse links |

### Content indexing

Hubs can also host content indexes — hash-based directories that map content hashes to server locations. Use `mark_index` to crawl a server and publish its content index to a hub, and `mark_resolve` to look up content by hash.

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
