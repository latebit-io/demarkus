# Single-Host Stack (Server + Broker + Library)

Run the full demarkus experience (world server, auth/issuance broker, and the web reading room) on one Linux VPS with systemd. No Kubernetes required: the broker runs in **file-backend mode**, writing token hashes directly to the `tokens.toml` the server already watches and hot-reloads.

## Quick start

```bash
# Server + read-only reading room (no auth stack)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | sudo bash -s -- --domain example.com --with-library

# Full stack with SSO (Google/GitHub OAuth app required, see below)
sudo install -m 600 /dev/null /root/oidc-secret
sudoedit /root/oidc-secret     # paste the OAuth client secret, save, quit
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | sudo bash -s -- --domain example.com --with-broker --with-library \
    --broker-public-url https://broker.example.com \
    --broker-oidc-issuer https://accounts.google.com \
    --broker-oidc-client-id <client-id> \
    --broker-oidc-client-secret-file /root/oidc-secret
```

The secret rides a root-readable file, never argv or a shell command line (both leak via `ps` and history). Omit the OIDC flags to scaffold the broker config for later editing; the unit installs **disabled** and the summary prints the two files to complete plus the `systemctl enable --now` to run.

Version pinning: `--version` pins the server; `--library-version` pins the reading room; the broker always installs from the same tools release as the other stack binaries. Client and tools otherwise resolve to their latest releases; the modules are versioned independently by design.

## What gets installed

| Component | Service | Ports | Config |
|---|---|---|---|
| World server | `demarkus` | UDP 6309 (public) | `/etc/demarkus/` |
| Broker | `demarkus-knowledge-broker` | loopback TCP 8080 (OAuth/API), 8081 (MCP gateway) | `/etc/demarkus-knowledge-broker/` |
| Library | `demarkus-library` | TCP 443 (HTTPS, when TLS is configured) or TCP 8090 | `/etc/demarkus-library/` |

The broker binds **loopback only**; its advertised `publicURL` is served by your TLS reverse proxy (see below). With a broker installed, `tokens.toml` moves to `/etc/demarkus/tokens/`, a subdirectory the broker owns, so its atomic writes never require access to the rest of `/etc/demarkus` (where the TLS keys live). This layout is sticky: later reinstalls keep it even without `--with-broker`.

**Removing the stack.** `demarkus-install uninstall` tears down every component (server, broker, library): services, users, binaries, and the broker's config and state directories (which hold credential-bearing artifacts). It verifies each removal and exits non-zero if anything could not be removed, so a partial teardown is never reported as complete. To keep the server but revert to server-only token handling: `systemctl disable --now demarkus-knowledge-broker`, move `/etc/demarkus/tokens/tokens.toml` back to `/etc/demarkus/tokens.toml` (restoring `root:demarkus` 640), and re-run the installer.

Each component runs as its own hardened system user (`ProtectSystem=strict`, minimal `ReadWritePaths`).

## Which tier do you need?

Demarkus onboarding splits by what you want to do, not by where you deploy. Pick the smallest tier that covers your case.

| You want to... | Setup | IdP needed? |
|---|---|---|
| **Read** documents (browser or client) | library in quic mode, or any client (`demarkus`, TUI, MCP) | no |
| **Write** from the CLI or an agent | a capability token (`demarkus-token generate`), delivered as a [join URL](../tools/index.md) | no |
| **Onboard people to a shared knowledge system** | broker + OIDC; users run `/knowledge-join <url>` and log in | yes |
| **Edit in the browser** (the library cataloging desk) | library in **broker mode** + OIDC | yes |

Only the last two need an identity provider, and only browser editing forces it: the library's cataloging desk writes exclusively through broker mode (the direct-QUIC path is read-only by design). Reading and tool-based writing never need an IdP.

### "An IdP" does not mean Google

The broker speaks standard OIDC, so **any** provider works, including a self-hosted one. If you do not want a commercial IdP in the loop, run [Dex](https://dexidp.io/) (a single small container), [Keycloak](https://www.keycloak.org/), or [Authentik](https://goauthentik.io/) and point `oidc.issuer` at it. This keeps a browser-editable, multi-user deployment fully self-hosted with no central authority.

## How single-host mode works

The broker's `storage.backend: file` replaces its Kubernetes Secret storage:

- **World tokens**: the broker provisions one write token per world and appends its hash to that world's `tokensFile` (the server's `tokens.toml`). The server's file watcher picks the change up immediately; no signal, no restart.
- **Broker state** (refresh tokens, write-token records) lives under `/var/lib/demarkus-knowledge-broker/`, so logins survive broker restarts.
- **Ownership**: the broker owns `tokens.toml` (group `demarkus`, mode 640) so its atomic replacement writes need no privilege while the server keeps group-read. The installer sets this up.

The generated `/etc/demarkus-knowledge-broker/config.yaml` registers the local world as `soul` with `internalAddress: localhost:6309`. Add more worlds (more servers on other ports or hosts) as additional entries.

## OIDC application setup

Register an OAuth app at your IdP, a commercial one (Google Cloud Console, GitHub Developer Settings, Okta) or a self-hosted one (Dex, Keycloak, Authentik):

- **Redirect URI**: `<broker-public-url>/auth/callback`; must match `oidc.redirectURL` exactly.
- The client secret never lives in `config.yaml`: it rides `OIDC_CLIENT_SECRET` in `/etc/demarkus-knowledge-broker/env` (mode 640).
- Restrict who may log in with `oidc.allowDomains` (hosted-domain allowlist) and per-world `allow` lists.

For a fully self-hosted stack, run Dex alongside the broker (it needs only a static config and a listen port), register the broker as a Dex client, and set `oidc.issuer` to the Dex URL. Everything else on this page is unchanged.

Agents join with `/knowledge-join <broker-public-url>` from the demarkus-knowledge plugin.

## Library modes

`--with-library` installs the reading room in **direct-QUIC (read-only) mode**: no login, serving your world's documents. This works with or without the broker.

The port and scheme follow the install's TLS setup: with TLS configured (`--domain` or `--tls-cert`/`--tls-key`) the library serves **HTTPS on 443** using the world's certificate: same hostname, same identity, so `https://<domain>/` is the reading room. Without TLS it serves plain HTTP on 8090. `--library-port <n>` overrides either default; the installer refuses a port something else already listens on. On reinstalls an existing `/etc/demarkus-library/env` keeps its `PORT`, so upgrading an old 8090 install to 443 is an env edit (`PORT=443`, plus the `DEMARKUS_TLS_CERT`/`DEMARKUS_TLS_KEY` lines) followed by re-running the installer.

To put the library behind broker SSO (org login, per-reader identity):

1. Generate a client secret: `openssl rand -hex 32`, and its hash: `printf '%s' "<secret>" | sha256sum`.
2. Register the library in `/etc/demarkus-knowledge-broker/config.yaml`:

   ```yaml
   webClients:
     - clientID: library
       clientSecretHash: <sha256-hex>
       redirectURIs: ["https://<library-host>/auth/callback"]
       name: reading room
   ```

3. Switch `/etc/demarkus-library/env` to the broker block (uncomment and fill the `DEMARKUS_TRANSPORT=broker` lines with the plaintext secret).
4. Redirect URIs must be **absolute https URLs**; already satisfied on TLS installs, where the library serves HTTPS on 443 with the world's cert. Otherwise front it with TLS (its own `DEMARKUS_TLS_CERT`/`DEMARKUS_TLS_KEY`, or a reverse proxy such as Caddy/nginx).
5. `systemctl restart demarkus-knowledge-broker demarkus-library`.

## TLS and the reverse proxy

- The world server's TLS is handled by the main installer (Let's Encrypt with `--domain`, or `--tls-cert/--tls-key`).
- The broker's listeners are loopback plaintext; `publicURL` is the HTTPS origin your reverse proxy serves. A Caddyfile is the two-liner version:

  ```text
  broker.example.com {
      reverse_proxy 127.0.0.1:8080
  }
  ```

  Route the MCP gateway (`127.0.0.1:8081`) from the same or a second hostname as you prefer; `/knowledge-join` and the library talk to whatever `publicURL` resolves to.
- With a Let's Encrypt domain the generated config dials the world at `<domain>:6309` with verification on (the certificate can never match `localhost`); with a self-signed cert it dials `localhost:6309` with `worldDialer.insecureSkipVerify: true`.
- Dialing the public domain from the host itself requires hairpin NAT, which home routers often lack (external clients work, same-host components see the world as unreadable). The installer probes this after the world starts and, when the dial fails, pins the domain to loopback in `/etc/hosts`; the dial then bypasses NAT while the certificate name still matches. A failed probe with the pin already in place aborts the install with diagnostics instead.

## Verify the stack

```bash
systemctl status demarkus demarkus-knowledge-broker demarkus-library
curl -s http://localhost:8080/healthz     # broker
curl -s -o /dev/null -w "%{http_code}\n" https://<domain>/        # library on TLS installs (302 to the reading room)
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8090/   # library on no-TLS installs
demarkus --insecure mark://localhost:6309/index.md                # world
```
