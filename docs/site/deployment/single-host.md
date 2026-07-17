# Single-Host Stack (Server + Broker + Library)

Run the full demarkus experience — world server, auth/issuance broker, and the web reading room — on one Linux VPS with systemd. No Kubernetes required: the broker runs in **file-backend mode**, writing token hashes directly to the `tokens.toml` the server already watches and hot-reloads.

## Quick start

```bash
# Server + read-only reading room (no auth stack)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | sudo bash -s -- --domain example.com --with-library

# Full stack with SSO (Google/GitHub OAuth app required, see below)
echo -n '<client-secret>' > /root/oidc-secret && chmod 600 /root/oidc-secret
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
  | sudo bash -s -- --domain example.com --with-broker --with-library \
    --broker-public-url https://broker.example.com \
    --broker-oidc-issuer https://accounts.google.com \
    --broker-oidc-client-id <client-id> \
    --broker-oidc-client-secret-file /root/oidc-secret
```

The secret rides a root-readable file, not argv (argv is visible in `ps` and shell history). Omit the OIDC flags to scaffold the broker config for later editing; the unit installs **disabled** and the summary prints the two files to complete plus the `systemctl enable --now` to run. Pin the reading room with `--library-version <x.y.z>` for reproducible installs (the broker is always the same version as the rest of the stack's tools release).

## What gets installed

| Component | Service | Ports | Config |
|---|---|---|---|
| World server | `demarkus` | UDP 6309 (public) | `/etc/demarkus/` |
| Broker | `demarkus-broker` | loopback TCP 8080 (OAuth/API), 8081 (MCP gateway) | `/etc/demarkus-broker/` |
| Library | `demarkus-library` | TCP 8090 | `/etc/demarkus-library/` |

The broker binds **loopback only** — its advertised `publicURL` is served by your TLS reverse proxy (see below). With a broker installed, `tokens.toml` moves to `/etc/demarkus/tokens/`, a subdirectory the broker owns, so its atomic writes never require access to the rest of `/etc/demarkus` (where the TLS keys live).

Each component runs as its own hardened system user (`ProtectSystem=strict`, minimal `ReadWritePaths`).

## How single-host mode works

The broker's `storage.backend: file` replaces its Kubernetes Secret storage:

- **World tokens**: the broker provisions one write token per world and appends its hash to that world's `tokensFile` (the server's `tokens.toml`). The server's file watcher picks the change up immediately; no signal, no restart.
- **Broker state** (refresh tokens, write-token records) lives under `/var/lib/demarkus-broker/`, so logins survive broker restarts.
- **Ownership**: the broker owns `tokens.toml` (group `demarkus`, mode 640) so its atomic replacement writes need no privilege while the server keeps group-read. The installer sets this up.

The generated `/etc/demarkus-broker/config.yaml` registers the local world as `soul` with `internalAddress: localhost:6309`. Add more worlds (more servers on other ports or hosts) as additional entries.

## OIDC application setup

Register an OAuth app at your IdP (Google Cloud Console, GitHub Developer Settings, Okta, ...):

- **Redirect URI**: `<broker-public-url>/auth/callback` — must match `oidc.redirectURL` exactly.
- The client secret never lives in `config.yaml`: it rides `OIDC_CLIENT_SECRET` in `/etc/demarkus-broker/env` (mode 640).
- Restrict who may log in with `oidc.allowDomains` (hosted-domain allowlist) and per-world `allow` lists.

Agents join with `/knowledge-join <broker-public-url>` from the demarkus-knowledge plugin.

## Library modes

`--with-library` installs the reading room in **direct-QUIC (read-only) mode**: no login, serving your world's documents at port 8090. This works with or without the broker.

To put the library behind broker SSO (org login, per-reader identity):

1. Generate a client secret: `openssl rand -hex 32`, and its hash: `printf '%s' "<secret>" | sha256sum`.
2. Register the library in `/etc/demarkus-broker/config.yaml`:

   ```yaml
   webClients:
     - clientID: library
       clientSecretHash: <sha256-hex>
       redirectURIs: ["https://<library-host>/auth/broker/callback"]
       name: reading room
   ```

3. Switch `/etc/demarkus-library/env` to the broker block (uncomment and fill the `DEMARKUS_TRANSPORT=broker` lines with the plaintext secret).
4. Redirect URIs must be **absolute https URLs** — front the library with TLS (its own `DEMARKUS_TLS_CERT`/`DEMARKUS_TLS_KEY`, or a reverse proxy such as Caddy/nginx).
5. `systemctl restart demarkus-broker demarkus-library`.

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

## Verify the stack

```bash
systemctl status demarkus demarkus-broker demarkus-library
curl -s http://localhost:8080/healthz     # broker
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8090/   # library (302 to the reading room)
demarkus --insecure mark://localhost:6309/index.md                # world
```
