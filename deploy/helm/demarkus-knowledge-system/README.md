# demarkus-knowledge-system Helm chart

One values file installs a knowledge system: the multi-world
`demarkus-knowledge-server`, the `demarkus-knowledge-broker` (OIDC login plus
the MCP gateway), the `demarkus-agent` federation crawler and, optionally,
the `demarkus-library` reading room. Each world is declared once under
`global.worlds`; every sub-chart derives its own entries from that list.

## Install

```yaml
# knowledge-system.yaml
global:
  bucketPrefix: gs://my-project-demarkus-
  worlds:
    - name: root
      worldID: 1bb83179-7275-41a2-9519-a3410a7ac5dd
      hub: true
    - name: team-a
      worldID: d7867876-1369-4b39-a33d-b175a4320acf
      allow:
        groups: ["team-a"]

knowledge:
  serviceAccount:
    workloadIdentity:
      gsa: demarkus-knowledge@my-project.iam.gserviceaccount.com

broker:
  server:
    publicURL: https://broker.example.com
  oidc:
    issuer: https://accounts.google.com
    clientID: YOUR_CLIENT_ID
    existingSecretRef:
      name: broker-oidc
    redirectURL: https://broker.example.com/auth/callback
  ingress:
    enabled: true
    host: broker.example.com
    tls:
      certManager:
        enabled: true
  webClients:
    - clientID: library-web
      existingSecretRef:
        name: library-oauth
      redirectURIs:
        - https://library.example.com/auth/callback

library:
  enabled: true
  library:
    broker:
      url: https://broker.example.com
      redirectURI: https://library.example.com/auth/callback
  ingress:
    enabled: true
    host: library.example.com
```

```sh
kubectl create namespace demarkus
kubectl -n demarkus create secret generic broker-oidc --from-literal=clientSecret=...
kubectl -n demarkus create secret generic library-oauth --from-literal=clientSecret="$(openssl rand -hex 32)"
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --values knowledge-system.yaml
```

Two Secrets, both yours: the IdP client secret and the library's client
secret (the broker hashes it at load, so it is shared, not duplicated). The
broker generates and persists its own signing key on first start, the
knowledge server bootstraps one tokens Secret per world, and the agent
projects the hub's raw token from there. Buckets and the GSA are the only
out-of-cluster prerequisites.

## What derives from `global`

| Global | Knowledge server | Broker | Agent |
| --- | --- | --- | --- |
| `worlds[].name` | world, `<name>-tokens` Secret | world, per-world RBAC, `<name>-tokens` | seed (`mark://<name>`) |
| `worlds[].worldID` | `bucket.worldID` | | |
| `worlds[].hub` | | | hub target, `<hub>-token-values` publish token |
| `worlds[].allow` | | per-world predicate | |
| `worlds[].readOnly`, `initialPolicy`, `limits` | as is | | |
| `bucketPrefix` | `bucket.url = <prefix><name>` | | |
| `knowledgeService` | resource and Service name | `dialAddress` | `dial_address` |
| `authorityDomain` | authorities `<name>.<domain>` (certificate SANs) | `internalAddress` `<name>.<domain>:6309` | `server_name` |

`authorityDomain` defaults to `<knowledgeService>.<release namespace>.svc.cluster.local`.
It only has to agree between the three charts; it never resolves in DNS,
because the broker and agent dial the shared Service directly and present
the authority as SNI. No ExternalName alias Services are needed.

Any field set explicitly on a sub-chart overrides its derived value, so a
world with a hand-managed bucket or a legacy per-namespace server still fits.

## What does not derive

The library chart lives in another repository and reads none of `global`;
its broker wiring under `library.library.broker` is explicit. Its
`existingSecretRef` should name the same Secret as the broker's `webClients`
entry.

## Defaults worth knowing

- Sub-chart resources are named `knowledge` (from `global.knowledgeService`),
  `broker`, `agent` and `library` (through `fullnameOverride`).
- The knowledge server renders a self-signed cert-manager `Issuer`; the
  broker (`worldDialer.insecureSkipVerify`) and agent (`insecure`) skip
  verification. Replace with a CA issuer and flip both once one exists.
- NetworkPolicies are off (the standalone charts default them on). Enabling
  the knowledge server's policy under
  the umbrella also needs `knowledge.networkPolicy.broker.namespace` and
  `agent.namespace` set to the release namespace; pod labels already match.
- The knowledge server runs two replicas and refuses fewer.

## Tests

```sh
helm dependency update deploy/helm/demarkus-knowledge-system
helm unittest deploy/helm/demarkus-knowledge-system
```
