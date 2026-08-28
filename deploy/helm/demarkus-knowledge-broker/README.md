# demarkus-knowledge-broker Helm chart

OIDC token broker for demarkus. Exchanges a verified IdP identity for one
or more demarkus tokens, writing token hashes into per-world Kubernetes
Secrets.

## What this chart ships

- Multi-replica `Deployment` (default 2) with `RollingUpdate` strategy
  and `PodDisruptionBudget` (`minAvailable: 1`).
- Chart-managed broker config Secret with auto-generated cookie HMAC key
  preserved across `helm upgrade` via `lookup`.
- Per-world `Role` + `RoleBinding` in each world's namespace
  (`get/update` on the world's tokens Secret) plus broker-namespace
  `Role` covering the sweeper Lease, the refresh-tokens Secret, and
  `create` + per-world `get/update` on the write-token Secrets the
  broker provisions on first write.
- Default-on `NetworkPolicy` restricting ingress to the configured
  Ingress controller namespace and egress to DNS, TCP 443, and each
  configured world UDP port (6309 by default).
- Locked-down pod security context: nonroot UID, read-only root
  filesystem, all capabilities dropped, seccomp RuntimeDefault.
- Optional `Ingress` (default `ingressClassName: nginx`) with optional
  cert-manager-managed TLS Certificate.
- **MCP gateway** (always-on, listens on `:8081` by default) exposing
  the 14-tool demarkus surface to plugin-style agents over JSON-RPC
  over Streamable HTTP. Identity is the company SSO `id_token` bearer;
  reads dispatch with no token (open to any SSO-authed identity) and
  writes use a long-lived per-world token the broker holds. See
  [MCP gateway](#mcp-gateway).
- TopologySpreadConstraints to keep replicas off the same node.

## Endpoint surface

The broker exposes two distinct HTTP listeners on the pod, fronted by
the same Ingress controller through different hostnames:

| Listener | Default port | Default Ingress host | Purpose |
| --- | --- | --- | --- |
| Management API | `:8080` (`server.port`) | `ingress.host` | OIDC login + device flow + token management + `/me/install` (see table below). |
| MCP gateway | `:8081` (`server.mcp.addr`) | `ingress.mcp.host` (optional) | 14-tool demarkus surface over JSON-RPC/Streamable HTTP for plugin agents. See [MCP gateway](#mcp-gateway). |

The two listeners share auth (`compositeVerifier`), rate-limit
buckets (per-canonical-email), and the `Issuer` machinery; only the
wire shape (JSON-RPC vs REST) differs.

## Management API endpoint surface

| Method | Path | Auth | Notes |
| --- | --- | --- | --- |
| `GET` | `/healthz`, `/readyz` | none | Liveness / readiness probes. |
| `GET` | `/.well-known/openid-configuration` | none | Broker discovery doc; proxies the IdP's doc with `issuer`, `device_authorization_endpoint`, `token_endpoint`, and `jwks_uri` rewritten to the broker. 5-minute TTL. |
| `GET` | `/.well-known/jwks.json` | none | Public ECDSA P-256 key for broker-signed `id_token`s (refresh-grant output). |
| `GET` | `/auth/login` | none | OIDC code-flow login redirect. IP rate-limited. |
| `GET` | `/auth/callback` | OIDC state cookie | OIDC code-flow callback; mints per-world tokens; dispatches device-flow handoffs by signed state. |
| `POST` | `/device/authorize`, `/device/token` | none | RFC 8628 device flow. IP rate-limited. `/device/token` accepts both `grant_type=urn:ietf:params:oauth:grant-type:device_code` and `grant_type=refresh_token`. |
| `GET`, `POST` | `/device` | none | HTML user-code entry form. |
| `POST` | `/token/revoke` | possession | RFC 7009 refresh-token revocation. IP rate-limited. |
| `GET` | `/me/install` | Bearer id_token | Per-user install bundle (universe onboarding). Per-subject rate-limited. |

### `/me/install`

Returns the caller's identity and installable world metadata; one
entry per world the verified identity is authorized for AND that has
a non-empty `publicURL` configured. The endpoint mints no world
tokens and returns no raw token material; world access is mediated
entirely by the MCP gateway (reads are open to any SSO-authed
identity, writes use a broker-held per-world write token).

```http
GET /me/install
Authorization: Bearer <id_token>

200 OK
Content-Type: application/json
Cache-Control: no-store
Pragma: no-cache
{
  "email": "alice@example.com",
  "worlds": [
    {
      "name": "team-a",
      "publicURL": "mark://team-a.example:6309"
    }
  ]
}
```

Notes:
- **Worlds without `publicURL` are excluded.** A world with an empty
  `worlds[].publicURL` is structurally un-installable (the client has
  no address to wire) so the broker filters it from the bundle.
- **Empty `worlds: []` is a 200, not a 403.** An authenticated identity
  with zero installable worlds (no allowlist match, or every match
  filtered by the `publicURL` rule) returns `200 + worlds: []` so
  consumers can distinguish auth failure from authz emptiness.
- **No token material is returned.** The response carries identity +
  world metadata only; the broker mints nothing on this path, so there
  is no per-world token, expiry, or partial-mint-failure shape.
- **The bearer accepts both broker-signed and IdP-signed id_tokens.**
  Broker-signed tokens come from the device-flow refresh grant; IdP-
  signed tokens come from the device-flow completion. The broker's
  composite verifier dispatches by `kid`.

## MCP gateway

The MCP gateway is the broker's HTTPS-fronted JSON-RPC surface for
plugin-style agents (Claude Code, IDE integrations, anything
speaking the Model Context Protocol). One MCP server entry on the
client connects the agent to every world the operator has
configured under `worlds[]`: no per-world client setup. The
gateway translates HTTPS/JSON-RPC at the org boundary into QUIC/Mark
Protocol inside the cluster, so corporate networks that block UDP
reach the universe over standard `:443`.

### Always-on

The gateway is part of the binary, not a feature flag. Every broker
listens on `server.mcp.addr` (default `:8081`); the chart's Service
exposes the port as `mcp`; the NetworkPolicy admits ingress on it.
Existing deployments upgrading the chart pick up the listener
silently; no `mcp.enabled` knob to set. To keep the listener
cluster-internal, leave `ingress.mcp.host` blank; the port is still
admitted from the ingress-controller namespace by the
NetworkPolicy but no external rule routes traffic to it.

### Hostname topology

The MCP gateway routes through a SEPARATE hostname from the
management API. The chart's `ingress.yaml` adds a second rule + TLS
block to the same Ingress resource when `ingress.mcp.host` is set,
backed by the Service's `mcp` port.

```yaml
ingress:
  enabled: true
  host: broker.acme.com          # management API
  tls:
    certManager:
      enabled: true              # cert-manager mints broker.acme.com TLS
  mcp:
    host: mcp.broker.acme.com    # MCP gateway
    tls:
      certManager:
        enabled: true            # cert-manager mints mcp.broker.acme.com TLS
```

Splitting the hosts avoids `.well-known/*` path collisions: the
management API (the issuer origin) serves
`/.well-known/openid-configuration`,
`/.well-known/oauth-authorization-server` (RFC 8414; must live on
the issuer's origin per §3.3, strict clients validate issuer ==
fetch origin) and `/.well-known/jwks.json`, while the MCP gateway
serves only `/.well-known/oauth-protected-resource` (RFC 9728,
bare and path-inserted `/mcp` forms). Set `server.mcp.publicURL`
to the gateway host so the `resource` field and the 401
`resource_metadata` link name the gateway, not the issuer. Two
hostnames, two TLS contexts, no path-routing fragility, and
operators rotate certs on either side independently.

### TLS termination

Two supported modes. **Pick one per surface.**

| Mode | Operator action | Where TLS terminates |
| --- | --- | --- |
| **Ingress-terminated** (recommended) | Configure `ingress.mcp.tls.{existingSecret,certManager}` as above. Leave `server.mcp.tls.existingSecretRef.name` blank. | At the Ingress controller. Broker speaks plain HTTP inside the cluster. Matches the management API's posture. |
| **Broker-terminated** | Provision a `kubernetes.io/tls` Secret in the broker's namespace and set `server.mcp.tls.existingSecretRef.name=<secret>`. | At the broker pod. Chart mounts the Secret read-only at `/etc/demarkus-knowledge-broker/tls/mcp/` and points `MCPConfig.TLS.{CertFile,KeyFile}` at the projected `tls.crt` / `tls.key`. Use when the topology bypasses the Ingress or requires mTLS broker→Ingress. |

There is intentionally no in-line PEM mode (`brokerSigningKey`-style
cleartext) for the MCP listener: a TLS private key in helm release
history is never acceptable, even for dev. Use `existingSecretRef`
or terminate at the Ingress.

### Plugin-side flow

The next slice ships a `/knowledge-join` slash command in the
`demarkus-memory` Claude Code plugin. Operators direct users to:

```
/knowledge-join https://mcp.broker.acme.com
```

The plugin validates the broker URL by fetching
`/.well-known/oauth-protected-resource`, derives a per-org slug from
the hostname, and runs `claude mcp add --transport http {slug}
{url}/mcp`. The Claude Code OAuth device flow handles the first-time
identity exchange against the broker's existing
`/device/authorize` + `/device/token` endpoints (universe-onboarding
PR3+PR4). After that the plugin sees all 13 demarkus tools as one
MCP server.

### OAuth flow shape

The gateway implements MCP's [authorization
spec](https://modelcontextprotocol.io/specification/2024-11-05/basic/authorization)
using the broker's existing OIDC machinery:

- `GET /.well-known/oauth-protected-resource` (RFC 9728, also the
  path-inserted `/mcp` form); declares the resource server
  (`resource` = gateway host `/mcp`) and points clients at the
  authorization server (`authorization_servers` = the management
  host, i.e. `server.publicURL`).
- `GET /.well-known/oauth-authorization-server` (RFC 8414); served
  on the **management host**, aliasing the OIDC Discovery handler
  (`/.well-known/openid-configuration`): §3.3 requires the metadata
  on the issuer's own origin, so the gateway deliberately does not
  serve it. The actual IdP (Google, Okta, Entra) is one hop deeper,
  handled by the broker's existing PR3 device-flow + PR4
  refresh-grant code.
- `POST /mcp`: single JSON-RPC endpoint. Unauthenticated requests
  receive `401 + WWW-Authenticate: Bearer
  resource_metadata="...oauth-protected-resource"` per RFC 6750 +
  RFC 9728, so a fresh client knows where to start the auth flow.
- All authenticated tool calls present `Authorization: Bearer
  <id_token>`. The broker validates via the existing PR4
  `compositeVerifier` (accepts both broker-signed and IdP-signed
  tokens), extracts `email` + `email_verified`, **rejects unverified
  emails** (matches `ErrEmailUnverified`), and canonicalizes the
  email (trim + lowercase) for per-subject rate-limit keying.

### Rate-limit behavior

The gateway's rate limiter is the same per-canonical-email bucket
the management API's `/me/install` route uses. A single user calling
both the MCP gateway and the management API shares one bucket; an
abusive identity cannot multiply its effective rate by fanning out
across surfaces. Defaults from `rateLimit.tokens`: 10/min per
email, burst 5. The 401 + 429 envelope shape is the same on either
listener.

### Ephemeral graph store

**The broker's graph store (used by `mark_backlinks`, `mark_graph`,
`mark_graph_export`, `mark_graph_publish`) lives in process memory
and does NOT survive broker pod restarts.** A rolling chart upgrade,
node drain, or pod eviction drops every cached graph. The first
user to call a graph-store tool after restart triggers a fresh
crawl via `mark_graph` (or whatever federation tool needs the
data); the broker re-populates and continues. Operators planning
maintenance windows should expect a few seconds of cold-start
latency on the first graph-tool call after a bounce, and document
the re-crawl as expected behavior for end users.

This is a deliberate trade-off: a stateless broker pod is restart-
resilient, multi-replica-safe, and matches the gateway's framing as
a wire-shape adapter rather than a persistent protocol surface. A
bucket-store-backed persistent graph is on the radar for the
post-broker design window (see `/thoughts.md` § "On Bucket Stores")
but is not part of the chart today.

### Write-token provisioning + propagation retries

The broker keeps one long-lived write token per world. The raw token
is stored canonically in a broker-namespace Secret and cached
in-process; there is no per-email, in-memory session cache.

The `firstMintMax*` / `firstMintInitialBackoff` / `firstMintMaxBackoff`
knobs govern a broker-side retry loop that absorbs the kubelet →
world-Secret projection lag. When the broker first provisions a
world's write token, the world's projected `tokens.toml` volume may
not refresh before the next tool call, and the world will 401. The
retry loop re-dispatches the same token with exponential backoff
(default 6 attempts, 250ms → 8s) only on `unauthorized` responses
right after a world's write token is first provisioned. Writer
authorization is enforced at the broker before dispatch, so a
fresh-provision 401 is propagation lag, not a real denial. Defaults
sit well under the typical kubelet sync period; tune up only for
slow-kubelet clusters.

### Operator reference: 14-tool surface

See `tools/demarkus-knowledge-broker/MCP-API.md` in the repo for the full
tool reference (names, JSON schemas, semantics).

### Upgrade notes

Pre-gateway deployments upgrading this chart pick up the MCP
listener silently:

- Deployment grows a `mcp` containerPort on `:8081`.
- Service grows a `mcp` port routing to the named containerPort.
- NetworkPolicy admits ingress on the MCP port from the same
  ingress-controller namespace already configured for the
  management API.
- The rendered `config.yaml` carries the MCP block; existing
  deployments without operator overrides get the chart defaults.
- No new RBAC to enable the gateway: the broker-namespace `Role`
  already grants the write path everything it needs: `create` +
  per-world `get/update` on the write-token Secrets (broker namespace)
  and `get/update` on each world's tokens Secret (per-world `Role`).
  See "What this chart ships" above; nothing extra to provision.
- Worlds[] entries get an `internalAddress: ""` field. Empty string
  preserves the default `<name>.<namespace>.svc.cluster.local:6309`
  Service-DNS resolution the gateway uses for tool dispatch.
- External exposure requires setting `ingress.mcp.host` (and TLS).
  Without it, the listener stays cluster-internal.

## Quick install (development)

Put sensitive values in a values file rather than on the command line;
`--set` values leak into shell history and `ps` output. The values
file also keeps the multi-line broker signing PEM readable:

```yaml
# broker-secrets.values.yaml: do not commit
oidc:
  clientSecret: "YOUR_CLIENT_SECRET"
  # ECDSA P-256 key for broker-signed id_tokens. Generate with:
  #   openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out broker-key.pem
  brokerSigningKey: |
    -----BEGIN PRIVATE KEY-----
    YOUR_PEM_LINES_HERE
    -----END PRIVATE KEY-----
```

```bash
helm install broker deploy/helm/demarkus-knowledge-broker \
  --namespace demarkus-knowledge-broker --create-namespace \
  --values broker-secrets.values.yaml \
  --set server.publicURL=https://broker.example.com \
  --set oidc.issuer=https://accounts.google.com \
  --set oidc.clientID=YOUR_CLIENT_ID \
  --set oidc.redirectURL=https://broker.example.com/auth/callback \
  --set ingress.enabled=true \
  --set ingress.host=broker.example.com \
  --set ingress.tls.certManager.enabled=true \
  --set-json 'worlds=[{"name":"team-a","namespace":"team-a","tokensSecret":"team-a-tokens","allow":{"domains":["example.com"]},"defaultToken":{"paths":["/team-a/*"],"operations":["read","publish"],"expiresAfter":"24h"}}]'
```

World namespaces must already exist; the chart does not create them.

## Production checklist

Walk this list before exposing the broker to real traffic.

### 1. Use `oidc.existingSecretRef` for the OAuth client secret

The cleartext `oidc.clientSecret` values mode is fine for trials and
dev, but the value lives in helm release history (`helm get values`
shows it; `helm history` retains every revision). Production should
externalize the secret.

Create the Secret out of band, via External Secrets Operator, Sealed
Secrets, Vault, or `kubectl`:

```bash
kubectl create secret generic broker-oidc \
  --namespace demarkus-knowledge-broker \
  --from-literal=clientSecret=YOUR_CLIENT_SECRET
```

Then point the chart at it:

```yaml
oidc:
  clientSecret: ""              # leave blank; the env var supplies it
  existingSecretRef:
    name: broker-oidc
    key: clientSecret
```

The chart leaves `clientSecret` blank in the rendered config Secret and
mounts `OIDC_CLIENT_SECRET` into the broker pod via `secretKeyRef`.
The broker's `LoadConfig` applies the env-var override before
validating the config. Setting both `clientSecret` and
`existingSecretRef.name` fails template render with a clear error so
the precedence is never ambiguous.

### 2. Verify the X-Forwarded-For invariant for your Ingress controller

The chart defaults `rateLimit.trustForwardedFor: true` (inverting the
broker binary's safe-by-default `false`). The per-IP rate limiter on
`/auth/login` is only effective when the Ingress controller in front
of the broker **strips spoofed `X-Forwarded-For` from inbound requests
and appends the real client IP.**

| Controller     | Default behavior | Notes |
|----------------|------------------|-------|
| nginx-ingress  | Strips + appends | Default safe. `use-forwarded-headers` defaults to `false`, which makes the controller treat the inbound XFF as untrusted and overwrite it with the real client. |
| Traefik        | Strips + appends | Default safe when used as the entrypoint. `forwardedHeaders.trustedIPs` controls when Traefik will trust an upstream's XFF; leave empty for direct-from-internet. |
| HAProxy        | Configurable     | Set `option forwardfor` to append; older versions require `option http-pretend-keepalive` interactions to be checked. |
| Cloud LB (ALB/GCLB) | Provider-dependent | Most cloud LBs append the source IP to XFF but do NOT strip what the client sent. Pair with a Pod-level controller (nginx-ingress in front of the broker pods) that does strip. |

**If you're exposing the broker on a `NodePort`/`LoadBalancer` Service
without an Ingress in front, set `rateLimit.trustForwardedFor: false`.**
With trust enabled and no XFF-stripping proxy, an attacker can rotate
the header on every request to bypass the per-IP limit on
`/auth/login`. The chart's `NOTES.txt` flags this combination after
install.

### 3. Use real TLS, not the in-binary self-signed fallback

The broker binary speaks plain HTTP inside the cluster; TLS
termination is the Ingress controller's job. Two paths:

- `ingress.tls.existingSecret: <name>`: point at a pre-existing
  `kubernetes.io/tls` Secret (cert-manager-managed, externally
  provisioned, manually created from your CA, etc.). The chart only
  references the Secret; it does not create it.
- `ingress.tls.certManager.enabled: true`: chart provisions a
  cert-manager `Certificate` resource. Requires cert-manager installed
  in the cluster and a working `Issuer` / `ClusterIssuer`. Default
  issuer is `ClusterIssuer letsencrypt-prod`; override
  `ingress.tls.certManager.issuerRef` for staging-issuers or
  internal CAs.

### 4. Review the default NetworkPolicy constraints

The default NetworkPolicy relies on the automatic
`kubernetes.io/metadata.name` namespace label from Kubernetes 1.21+.
Older clusters must label the ingress-controller and `kube-system`
namespaces explicitly. OIDC and Kubernetes API egress is limited to
TCP 443. Mark Protocol egress includes UDP 6309 and custom ports parsed
from `worlds[].internalAddress`. Deployments using other OIDC or API ports must disable
`networkPolicy.enabled` and supply an equivalent custom policy.

### 5. Confirm the install-time RBAC works for your cluster

With `rbac.create: true` (default) the chart creates per-world
`Role` + `RoleBinding` in every world's namespace. The
ServiceAccount running `helm install` needs `Role`/`RoleBinding`
create rights in each of those namespaces.

On managed Kubernetes that typically means cluster-admin, or a custom
install-time `ClusterRoleBinding` granting RBAC create in the world
namespaces. If your cluster restricts cross-namespace RBAC
management, set `rbac.create: false` and provision the per-world
`Role`s out of band; every world entry needs `secrets`
`get/update` on its `tokensSecret`, plus the broker-namespace `Role`
covering `coordination.k8s.io/leases` + the refresh-tokens Secret.

### 6. Set sensible resource limits and confirm the PDB

The chart's defaults (`100m`/`128Mi` request, `500m`/`256Mi` limit)
are sized for the realistic ~10k-subjects worst-case in the
rate-limit registry doc-comment. Audit them against your actual
expected concurrency. The default
`podDisruptionBudget.minAvailable: 1` prevents a node drain from
taking both replicas down.

## Cookie key preservation

The signed-state-cookie HMAC key is generated on first install and
preserved across `helm upgrade` via a `lookup` of the live config
Secret. To rotate the key, set `server.cookieKey` to a new
base64-encoded value and restart the broker. Any in-flight OIDC login
is invalidated by rotation, which is the intended behavior.

## Values

See `values.yaml` for the full schema with inline documentation.
