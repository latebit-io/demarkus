# demarkus-knowledge-system Helm chart

One values file installs a knowledge system: `demarkus-knowledge-server`
(the multi-world server with the OIDC broker and its MCP gateways in one
process), the `demarkus-agent` federation crawler and, optionally, the
`demarkus-library` reading room. Each world is declared once under
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
without any token, and issues the agent's hub token into
`<hub>-token-values` (`knowledge.broker.agentTokens`), which the agent
projects. Buckets and the GSA are the only out-of-cluster prerequisites.

The render is the same under Argo CD or Flux: the knowledge chart's
bootstrap Job is off (`knowledge.tokens.bootstrap.enabled: false`), so no
hook, no kubectl image and no token Secret to seal. An install that ran the
Job keeps its `<hub>-token-values`; the broker leaves a Secret it did not
create alone. Delete that Secret to hand the token over to the broker.

## What derives from `global`

| Global | Knowledge (worlds and broker) | Agent |
| --- | --- | --- |
| `worlds[].name` | world, `<name>-tokens` and `<name>-static-tokens` Secrets, gateway world served in process | seed (`mark://<name>`) |
| `worlds[].worldID` | `bucket.worldID` | |
| `worlds[].hub` | `agentTokens` entry writing `<hub>-token-values` | hub target, `<hub>-token-values` publish token |
| `worlds[].allow`, `writeScope`, `publicURL` | gateway predicate, write scope, `/me/install` address | |
| `worlds[].readOnly`, `initialPolicy`, `limits` | as is | |
| `bucketPrefix` | `bucket.url = <prefix><name>`, tenant buckets | |
| `knowledgeService` | resource and Service name | `dial_address` |
| `authorityDomain` | authorities `<name>.<domain>` (certificate SANs, gateway SNI) | `server_name` |

`authorityDomain` defaults to `<knowledgeService>.<release namespace>.svc.cluster.local`.
It only has to agree between the two charts; it never resolves in DNS,
because the agent dials the shared Service directly and presents the
authority as SNI, and the gateways reach the worlds in process. No
ExternalName alias Services are needed.

Any field set explicitly on a sub-chart overrides its derived value, so a
world with a hand-managed bucket still fits.

## What does not derive

The library chart lives in another repository and reads none of `global`;
its broker wiring under `library.library.broker` is explicit. Its
`existingSecretRef` should name the same Secret as the broker's `webClients`
entry.

## Defaults worth knowing

- Sub-chart resources are named `knowledge` (from `global.knowledgeService`),
  `agent` and `library` (through `fullnameOverride`). The broker's state
  Secrets follow: `knowledge-refresh-tokens`, `knowledge-signing-key`,
  `knowledge-cookie-key`, `knowledge-dynamic-clients`.
- Every world lives in the release namespace, next to the pods that mount
  its token Secrets; a `namespace` on a `global.worlds` entry fails the
  render.
- The knowledge chart renders a self-signed cert-manager `Issuer` for QUIC;
  the agent (`insecure`) skips verification. Replace with a CA issuer and
  flip it once one exists. The broker's Ingress TLS is separate
  (`knowledge.ingress.tls`).
- NetworkPolicies are off (the standalone charts default them on). Enabling
  the knowledge chart's policy under the umbrella also needs
  `knowledge.networkPolicy.agent.namespace` set to the release namespace and
  `ingressFromNamespace` to your controller's; pod labels already match.
- The knowledge deployment runs two replicas and refuses fewer.

## Tests

```sh
helm dependency update deploy/helm/demarkus-knowledge-system
helm unittest deploy/helm/demarkus-knowledge-system
```
