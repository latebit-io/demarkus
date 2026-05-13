# demarkus-broker Helm chart

OIDC token broker for demarkus. Exchanges a verified IdP identity for one
or more demarkus tokens, writing token hashes into per-world Kubernetes
Secrets and tracking ownership in a broker-namespace issuances Secret.

## What this chart ships (chart-skeleton slice)

- Multi-replica `Deployment` (default 2) with `RollingUpdate` strategy.
- Chart-managed broker config Secret with auto-generated cookie HMAC key
  preserved across `helm upgrade` via `lookup`.
- Issuances Secret seeded empty on first install with
  `helm.sh/resource-policy: keep` so chart updates and uninstalls never
  wipe broker-minted issuance records.
- ServiceAccount with optional GKE Workload Identity annotation.
- Cluster-IP Service exposing the broker's HTTP listener.
- Pod-level + container-level security context locked down: nonroot UID,
  read-only root filesystem, all capabilities dropped, seccomp
  RuntimeDefault, no privilege escalation.
- TopologySpreadConstraints to keep replicas off the same node.
- Resource requests/limits sized for the realistic ~10k-subjects
  worst-case in the rate-limit registry doc-comment.

Following slices add: per-world RBAC + NetworkPolicy + PDB, Ingress +
TLS + production secret-ref plumbing, helm-unittest + kind integration
in CI.

## Quick install (development)

```bash
helm install broker deploy/helm/demarkus-broker \
  --namespace demarkus-broker --create-namespace \
  --set oidc.issuer=https://accounts.google.com \
  --set oidc.clientID=YOUR_CLIENT_ID \
  --set oidc.clientSecret=YOUR_CLIENT_SECRET \
  --set oidc.redirectURL=https://broker.example.com/auth/callback \
  --set-json 'worlds=[{"name":"team-a","namespace":"team-a","tokensSecret":"team-a-tokens","allow":{"domains":["example.com"]},"defaultToken":{"paths":["/team-a/*"],"operations":["read","publish"],"expiresAfter":"24h"}}]'
```

The world namespaces (`team-a` in the example) must already exist; the
chart does not create them. RBAC across world namespaces ships in the
RBAC slice.

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
  removing the chart, delete the Secret manually:
  `kubectl delete secret <release>-issuances -n <broker-namespace>`.
- Reinstalling the chart in the same namespace picks up the existing
  issuance state, so users keep their tokens across chart lifecycle
  events.

## Values

See `values.yaml` for the full schema with inline documentation.
