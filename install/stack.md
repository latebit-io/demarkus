---
layout: default
title: Five-Minute Appliance
permalink: /install/stack/
---

# The five-minute appliance

One command turns a fresh Linux host into a complete knowledge system: world
server, broker, [reading room](/library/), self-hosted OIDC, automatic HTTPS, and
the indexing agent.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | sudo bash
```

No flags, no DNS setup, nothing to sign up for.

## What it installs

| Component | Role |
|---|---|
| `demarkus-server` | The world: versioned markdown over QUIC |
| `demarkus-broker` | OIDC-fronted MCP gateway; agents join here |
| `demarkus-library` | Reading room, plus the AI librarian if you supply a key |
| `demarkus-agent` | Indexing agent; republishes `/graph.md` every 6 hours |
| Authelia | Self-hosted identity provider |
| Caddy | Automatic HTTPS via Let's Encrypt |

It drives the regular [`install.sh`](/install/linux/) for the demarkus
components, then layers identity and TLS on top.

## What you get

```text
https://library.<host>     reading room (read + edit)
https://broker.<host>      MCP gateway
https://auth.<host>        login
mark://soul.<host>:6309    the world, direct over QUIC
```

The install prints a card with the owner login and both join commands:

```text
Agents join with:   /knowledge-join https://broker.<host>
Personal memory:    /soul-join mark://soul.<host>:6309#token=<token>
```

## DNS

Without `--domain` the stack names itself `<public-ip>.sslip.io` — real DNS, real
Let's Encrypt certificates, zero setup. That identity follows the machine's IP, so
switch to a real domain once you're keeping the system.

With a domain, point one wildcard A record at the host:

```text
*.kb.example.com    A    <host ip>
```

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | \
  sudo bash -s -- --domain kb.example.com --owner-email you@example.com
```

## Options

| Flag | Meaning |
|---|---|
| `--domain <host>` | Base domain; `library.`, `broker.`, `auth.`, `soul.` hang off it |
| `--owner-email <addr>` | First user's email, and the Let's Encrypt contact |
| `--librarian-key-file <path>` | File holding the LLM key that enables the [librarian](/library/#the-librarian) |
| `--library-version <ver>` | Pin a library release instead of the latest |

The librarian key rides a `chmod 600` file, never argv, which leaks through `ps`
and shell history. A group- or world-readable key file is refused.

## Day two

```bash
# add users or change the owner password
sudoedit /etc/demarkus-auth/users.yml
sudo systemctl restart demarkus-auth

# enable the librarian later
echo 'LLM_API_KEY=...' | sudo tee -a /etc/demarkus-library/env
sudo systemctl restart demarkus-library

# remove everything
sudo demarkus-stack uninstall
```

Re-running the installer is safe: it preserves `users.yml`, keeps the configured
host, and re-converges the services.

Certificates provision on first request, so the first page load takes a few
seconds. Backlinks and the graph view populate after the agent's first crawl.

## Beyond one box

The appliance is the whole system on a single host. Same architecture at cluster
scale — many worlds, GitOps, external secrets, backups — is the [organizational
knowledge system](/scenarios/knowledge-system/), deployed from a forkable
template.
