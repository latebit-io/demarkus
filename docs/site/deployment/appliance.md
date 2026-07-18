# The Five-Minute Appliance

Stand up a complete, fully self-hosted knowledge system on one Linux VPS with a single command: world server, knowledge-system broker, web reading room with browser editing, a self-hosted identity provider (Authelia), automatic HTTPS (Caddy), and a background indexing agent. No Kubernetes, no domain, no OAuth app registration, no config files.

## Quick start

```bash
# On a fresh Ubuntu/Debian droplet (DigitalOcean or anywhere):
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | sudo bash
```

That is the whole thing. In about two minutes the installer prints a card:

```text
==> Your knowledge system is running

  Library (read + edit):  https://library.203.0.113.7.sslip.io
  Login:                  owner / k7mFq2c8...
                          (change it: /etc/demarkus-auth/users.yml, then restart demarkus-auth)
  Owner email:            owner@203.0.113.7.sslip.io
  Agents join with:       /knowledge-join https://broker.203.0.113.7.sslip.io
  Librarian AI:           off (add LLM_API_KEY to /etc/demarkus-library/env)
```

Open the library URL, log in with the printed credentials, and you have a working, versioned, browser-editable knowledge base with agent onboarding.

## No domain needed

Without `--domain`, the installer derives the host from the droplet's public IP via [sslip.io](https://sslip.io) wildcard DNS: `library.<ip>.sslip.io`, `broker.<ip>.sslip.io`, `auth.<ip>.sslip.io` all resolve automatically. It is real DNS, so Caddy issues real Let's Encrypt certificates. You configure nothing.

The sslip identity is tied to this machine's IP. When you are keeping the system, move to a real domain:

```bash
# One wildcard A record: *.kb.example.com -> this droplet's IP, then:
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh \
  | sudo bash -s -- --domain kb.example.com --owner-email you@example.com
```

## Options

| Flag | Effect |
|---|---|
| `--domain <d>` | Use `library.<d>` / `broker.<d>` / `auth.<d>` instead of sslip.io. Needs one wildcard A record. |
| `--owner-email <e>` | Owner account email (default `owner@<host>`). |
| `--librarian-key <k>` | Enable the AI librarian with this LLM API key (sets `LLM_API_KEY`). Off by default. |
| `--library-version <v>` | Pin the reading room version (default: latest). |

Everything else is generated: the owner password, all Authelia secrets, the broker and library OIDC client secrets, the agent's publish token. Nothing is prompted.

## What gets installed

| Component | Service | Bind | Role |
|---|---|---|---|
| World server | `demarkus` | UDP 6309 (public) | versioned markdown store |
| Broker | `demarkus-broker` | loopback :8080/:8081 | knowledge-system gateway + OIDC client |
| Library | `demarkus-library` | loopback :8090 | reading room + cataloging desk (browser editing) |
| Authelia | `demarkus-auth` | loopback :9091 | self-hosted OIDC provider |
| Caddy | `caddy` | :80/:443 (public) | automatic HTTPS, routes the three subdomains |
| Indexing agent | `demarkus-agent` | outbound | crawls the world, republishes `/graph.md` every 6h |

Every service is a hardened systemd unit (`ProtectSystem=strict`, minimal `ReadWritePaths`, its own system user). No container runtime is installed; every component is a native binary.

## Identity: self-hosted, no Google

The stack ships [Authelia](https://www.authelia.com/) as a native binary — no Google, GitHub, or any external identity provider. The owner account is created at install with a generated password; add teammates by editing `/etc/demarkus-auth/users.yml` (hash a password with `authelia crypto hash generate argon2`) and restarting `demarkus-auth`. Because the broker speaks standard OIDC, you can later point it at any other provider instead.

## After install

- **Change the owner password**: edit `/etc/demarkus-auth/users.yml`, then `systemctl restart demarkus-auth`.
- **Enable the librarian AI**: add `LLM_API_KEY=...` to `/etc/demarkus-library/env`, then `systemctl restart demarkus-library`.
- **Add agents**: from Claude Code, `/knowledge-join https://broker.<host>` and log in.
- **Uninstall everything**: `demarkus-stack uninstall`.

## Certificates on first load

Caddy provisions certificates lazily, so the very first request to each subdomain takes a few seconds while the certificate is issued. Subsequent loads are instant. If you are on sslip.io behind a strict firewall, make sure ports 80 and 443 are open (the installer opens them via `ufw` when present).

## Relation to the single-host page

The [single-host stack](single-host.md) page documents the manual, component-by-component path (server + broker + library, bring your own IdP and TLS). The appliance is that same stack assembled and wired automatically, with Authelia and Caddy added so there is nothing left to configure. Use the appliance to get running; use the single-host page to understand or customize the pieces.
