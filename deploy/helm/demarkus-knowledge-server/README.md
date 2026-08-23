# demarkus-knowledge-server

Production Helm chart for the multi-world `demarkus-knowledge-server`. One
Deployment serves every configured authority over a shared UDP Service and
stores each world in its own pre-provisioned GCS bucket.

## Prerequisites

- Kubernetes 1.25+
- GKE Workload Identity, or an existing Kubernetes ServiceAccount with equivalent identity
- One initialized GCS bucket and immutable world ID per world
- One existing token Secret per world
- One existing multi-SAN TLS Secret, or cert-manager and a suitable Issuer
- Policy document at `/.well-known/demarkus/policy.md` in every bucket

The chart never creates buckets, PVCs, token Secrets, or TLS Secrets. The
optional `Certificate` asks cert-manager to populate the referenced TLS Secret.

## Install

```yaml
tls:
  certManager:
    enabled: true
    issuerRef:
      kind: ClusterIssuer
      name: internal-ca

serviceAccount:
  workloadIdentity:
    gsa: demarkus-knowledge@project.iam.gserviceaccount.com

networkPolicy:
  allowUnrestrictedHTTPS: true

worlds:
  - name: team-a
    authorities:
      - team-a.example.com
      - team-a.knowledge.svc.cluster.local
    bucket:
      url: gs://deployment-team-a
      worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
    tokenSecret:
      name: team-a-tokens
      key: tokens.toml
```

```sh
helm upgrade --install knowledge ./deploy/helm/demarkus-knowledge-server \
  --namespace demarkus-knowledge --create-namespace --values production.yaml
```

When installing from a source checkout, set `image.tag` to a published server
release. Packaged OCI charts receive that release tag through `appVersion`.

Use `tls.existingSecret` instead of `tls.certManager` when certificate lifecycle
is managed elsewhere. The certificate must cover every configured authority.
cert-manager mode derives one deduplicated multi-SAN certificate from those
authorities.

After Secret rotation, send `SIGHUP` to each pod or restart the Deployment. The
projected files update automatically, but certificate reload is signal-driven.

## Security

Health endpoints listen on the private pod port only. No health Service is
created. NetworkPolicy permits UDP ingress only from configured broker and agent
namespace/pod selectors and optional `externalCIDRs`. A LoadBalancer remains
blocked from direct clients until those CIDRs are set. Egress permits cluster
DNS, TCP 443 for GCS, and GKE metadata-server endpoints.

GCS egress cannot be restricted to stable CIDRs with standard Kubernetes
NetworkPolicy. The built-in policy therefore requires explicit
`allowUnrestrictedHTTPS: true`. Otherwise disable it and provide an egress proxy
or CNI FQDN policy.

Each world token Secret is projected read-only into a distinct path. TLS and
configuration are also read-only. Pods run as UID/GID 65532 with a read-only
root filesystem, dropped capabilities, RuntimeDefault seccomp, node and zone
topology spread, rolling updates, and a default PDB.

## Validation

Template rendering fails for missing TLS, Workload Identity, world identity,
authority, bucket, or token Secret values. It also rejects fewer than two
replicas and duplicate world names, normalized authorities, buckets, world IDs,
or token Secret names.

```sh
helm lint ./deploy/helm/demarkus-knowledge-server --values production.yaml
helm unittest ./deploy/helm/demarkus-knowledge-server
helm template knowledge ./deploy/helm/demarkus-knowledge-server \
  --namespace demarkus-knowledge --values production.yaml
```
