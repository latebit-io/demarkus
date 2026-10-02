# demarkus-knowledge-system Helm chart

One values file installs a knowledge system: `demarkus-knowledge-server`
(the multi-world server with the OIDC broker, its MCP gateways and the
federation graph in one process) and, optionally, the `demarkus-library`
reading room. Each world is declared once under `global.worlds`; every
sub-chart derives its own entries from that list.

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
    publicURL: https://broker.example.com
    oidc:
      issuer: https://accounts.google.com
      clientID: YOUR_CLIENT_ID
      existingSecretRef:
        name: broker-oidc
      redirectURL: https://broker.example.com/auth/callback
    webClients:
      - clientID: library-web
        existingSecretRef:
          name: library-oauth
        redirectURIs:
          - https://library.example.com/auth/callback
  ingress:
    enabled: true
    host: broker.example.com
    tls:
      certManager:
        enabled: true

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
broker generates and persists its own signing key on first start, serves
writes through the gateway under each world's `allow` and `writeScope`
without any token, and derives the federation graph of every world into the
`hub` world's `/graph/worlds/` and `/graph.md`, in process and with no token.
Buckets and the GSA are the only out-of-cluster prerequisites.

The render is the same under Argo CD or Flux: the knowledge chart's
bootstrap Job is off (`knowledge.tokens.bootstrap.enabled: false`), so no
hook, no kubectl image and no token Secret to seal.

The `demarkus-agent` crawler is no longer part of this chart. It is still
published standalone, for crawling servers outside the system; give it a
publish token of your own as the knowledge chart's README describes, and
let it publish somewhere other than the hub's `/graph.md`.

## What derives from `global`

| Global | Knowledge (worlds and broker) |
| --- | --- |
| `worlds[].name` | world, `<name>-tokens` and `<name>-static-tokens` Secrets, gateway world served in process |
| `worlds[].worldID` | `bucket.worldID` |
| `worlds[].hub` | federation hub (`broker.federation.hub`), at most one world |
| `worlds[].allow`, `writeScope`, `publicURL` | gateway predicate, write scope, `/me/install` address |
| `worlds[].readOnly`, `initialPolicy`, `limits` | as is |
| `bucketPrefix` | `bucket.url = <prefix><name>`, tenant buckets |
| `knowledgeService` | resource and Service name |
| `authorityDomain` | authorities `<name>.<domain>` (certificate SANs, gateway SNI) |

`authorityDomain` defaults to `<knowledgeService>.<release namespace>.svc.cluster.local`.
It never resolves in DNS: direct clients dial the shared Service and present
the authority as SNI, and the gateways reach the worlds in process. No
ExternalName alias Services are needed.

Any field set explicitly on a sub-chart overrides its derived value, so a
world with a hand-managed bucket still fits.

## What does not derive

The library chart lives in another repository and reads none of `global`;
its broker wiring under `library.library.broker` is explicit. Its
`existingSecretRef` should name the same Secret as the broker's `webClients`
entry.

## Defaults worth knowing

- Sub-chart resources are named `knowledge` (from `global.knowledgeService`)
  and `library` (through `fullnameOverride`). The broker's Secrets
  follow: `knowledge-signing-key`, `knowledge-cookie-key`,
  `knowledge-oauth-state`. Refresh tokens and MCP host registrations live in
  `knowledge.broker.stateBucket`.
- Every world lives in the release namespace, next to the pods that mount
  its token Secrets; a `namespace` on a `global.worlds` entry fails the
  render.
- The knowledge chart renders a self-signed cert-manager `Issuer` for QUIC;
  direct clients skip verification. Replace with a CA issuer once one
  exists. The broker's Ingress TLS is separate (`knowledge.ingress.tls`).
- NetworkPolicies are off (the standalone charts default them on). Enabling
  the knowledge chart's policy under the umbrella also needs
  `knowledge.networkPolicy.agent.namespace` and `podLabels` naming your
  direct QUIC clients, and `ingressFromNamespace` your controller's.
- The knowledge deployment runs two replicas and refuses fewer.

## Tests

```sh
helm dependency update deploy/helm/demarkus-knowledge-system
helm unittest deploy/helm/demarkus-knowledge-system
```
