# demarkus-broker Helm chart

OIDC token broker for demarkus. Exchanges a verified IdP identity for one
or more demarkus tokens, writing token hashes into per-world Kubernetes
Secrets and tracking ownership in a broker-namespace issuances Secret.

## What this chart ships

- Multi-replica `Deployment` (default 2) with `RollingUpdate` strategy
  and `PodDisruptionBudget` (`minAvailable: 1`).
- Chart-managed broker config Secret with auto-generated cookie HMAC key
  preserved across `helm upgrade` via `lookup`.
- Issuances Secret seeded empty on first install with
  `helm.sh/resource-policy: keep` so chart updates and uninstalls never
  wipe broker-minted issuance records.
- Per-world `Role` + `RoleBinding` in each world's namespace
  (`get/update` on the world's tokens Secret) plus broker-namespace
  `Role` covering the sweeper Lease and the issuances Secret.
- Default-on `NetworkPolicy` restricting ingress to the configured
  Ingress controller namespace and egress to DNS + TCP 443.
- Locked-down pod security context: nonroot UID, read-only root
  filesystem, all capabilities dropped, seccomp RuntimeDefault.
- Optional `Ingress` (default `ingressClassName: nginx`) with optional
  cert-manager-managed TLS Certificate.
- TopologySpreadConstraints to keep replicas off the same node.

## Endpoint surface

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
| `GET` | `/tokens` | Bearer id_token | Caller's issuance records. Per-subject rate-limited. |
| `DELETE` | `/tokens/{label}` | Bearer id_token | Owner-only revoke. Per-subject rate-limited. |
| `POST` | `/tokens/{label}/rotate` | Bearer id_token | Owner-only rotate. Per-subject rate-limited. |
| `GET` | `/me/install` | Bearer id_token | Per-user install bundle (universe onboarding). Per-subject rate-limited. |

### `/me/install`

Returns the caller's full install bundle — one entry per world the
verified identity is authorized for AND that has a non-empty
`publicURL` configured. Each call mints a fresh access token per
returned world; raw token material is never recoverable from the
issuances Secret, so reuse is not possible. Old tokens stay valid
until their `expiresAt`; the sweeper retires them.

```http
GET /me/install
Authorization: Bearer <id_token>

200 OK
Content-Type: application/json
Cache-Control: no-store
Pragma: no-cache
{
  "worlds": [
    {
      "name": "team-a",
      "publicURL": "mark://team-a.example:6309",
      "label": "usr_b23fbc20",
      "accessToken": "<raw token>",
      "expiresAt": "2026-05-16T18:00:00Z"
    }
  ]
}
```

Notes:
- **Worlds without `publicURL` are excluded.** A world with an empty
  `worlds[].publicURL` is structurally un-installable (the client has
  no address to wire) so the broker filters it BEFORE mint — no
  issuance record is written for excluded worlds.
- **Empty `worlds: []` is a 200, not a 403.** An authenticated identity
  with zero installable worlds (no allowlist match, or every match
  filtered by the `publicURL` rule) returns `200 + worlds: []` so
  consumers can distinguish auth failure from authz emptiness.
- **Partial failures return 200.** If some worlds minted but a later
  one failed (k8s blip, RBAC denial on one world's tokens Secret), the
  response carries the successful entries plus a
  `"partialFailure": "one_or_more_worlds_failed"` field — same shape
  as `/auth/callback` for the equivalent condition.
- **The bearer accepts both broker-signed and IdP-signed id_tokens.**
  Broker-signed tokens come from the device-flow refresh grant; IdP-
  signed tokens come from the device-flow completion. The broker's
  composite verifier dispatches by `kid`.

## Quick install (development)

Put sensitive values in a values file rather than on the command line —
`--set` values leak into shell history and `ps` output. The values
file also keeps the multi-line broker signing PEM readable:

```yaml
# broker-secrets.values.yaml — do not commit
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
helm install broker deploy/helm/demarkus-broker \
  --namespace demarkus-broker --create-namespace \
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

World namespaces must already exist — chart does not create them.

## Production checklist

Walk this list before exposing the broker to real traffic.

### 1. Use `oidc.existingSecretRef` for the OAuth client secret

The cleartext `oidc.clientSecret` values mode is fine for trials and
dev, but the value lives in helm release history (`helm get values`
shows it; `helm history` retains every revision). Production should
externalize the secret.

Create the Secret out of band — via External Secrets Operator, Sealed
Secrets, Vault, or `kubectl`:

```bash
kubectl create secret generic broker-oidc \
  --namespace demarkus-broker \
  --from-literal=clientSecret=YOUR_CLIENT_SECRET
```

Then point the chart at it:

```yaml
oidc:
  clientSecret: ""              # leave blank — the env var supplies it
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
| Traefik        | Strips + appends | Default safe when used as the entrypoint. `forwardedHeaders.trustedIPs` controls when Traefik will trust an upstream's XFF — leave empty for direct-from-internet. |
| HAProxy        | Configurable     | Set `option forwardfor` to append; older versions require `option http-pretend-keepalive` interactions to be checked. |
| Cloud LB (ALB/GCLB) | Provider-dependent | Most cloud LBs append the source IP to XFF but do NOT strip what the client sent. Pair with a Pod-level controller (nginx-ingress in front of the broker pods) that does strip. |

**If you're exposing the broker on a `NodePort`/`LoadBalancer` Service
without an Ingress in front, set `rateLimit.trustForwardedFor: false`.**
With trust enabled and no XFF-stripping proxy, an attacker can rotate
the header on every request to bypass the per-IP limit on
`/auth/login`. The chart's `NOTES.txt` flags this combination after
install.

### 3. Use real TLS, not the in-binary self-signed fallback

The broker binary speaks plain HTTP inside the cluster — TLS
termination is the Ingress controller's job. Two paths:

- `ingress.tls.existingSecret: <name>` — point at a pre-existing
  `kubernetes.io/tls` Secret (cert-manager-managed, externally
  provisioned, manually created from your CA, etc.). The chart only
  references the Secret; it does not create it.
- `ingress.tls.certManager.enabled: true` — chart provisions a
  cert-manager `Certificate` resource. Requires cert-manager installed
  in the cluster and a working `Issuer` / `ClusterIssuer`. Default
  issuer is `ClusterIssuer letsencrypt-prod`; override
  `ingress.tls.certManager.issuerRef` for staging-issuers or
  internal CAs.

### 4. Confirm the install-time RBAC works for your cluster

With `rbac.create: true` (default) the chart creates per-world
`Role` + `RoleBinding` in every world's namespace. The
ServiceAccount running `helm install` needs `Role`/`RoleBinding`
create rights in each of those namespaces.

On managed Kubernetes that typically means cluster-admin, or a custom
install-time `ClusterRoleBinding` granting RBAC create in the world
namespaces. If your cluster restricts cross-namespace RBAC
management, set `rbac.create: false` and provision the per-world
`Role`s out of band — every world entry needs `secrets`
`get/update` on its `tokensSecret`, plus the broker-namespace `Role`
covering `coordination.k8s.io/leases` + the issuances Secret.

### 5. Set sensible resource limits and confirm the PDB

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

## Issuances Secret resource policy

The issuances Secret is created with `helm.sh/resource-policy: keep`.
Consequences:

- `helm upgrade` does not overwrite the broker's runtime writes.
- `helm uninstall` does not delete the Secret. To fully clean up after
  removing the chart, delete it manually:
  `kubectl delete secret <release>-issuances -n <broker-namespace>`.
- Reinstalling the chart in the same namespace picks up the existing
  issuance state, so users keep their tokens across chart lifecycle
  events.

## Values

See `values.yaml` for the full schema with inline documentation.
