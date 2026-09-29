# demarkus-knowledge-server

Production Helm chart for the multi-world `demarkus-knowledge-server`. One
Deployment serves every configured authority over a shared UDP Service and
stores each world in its own pre-provisioned GCS bucket.

## Prerequisites

- Kubernetes 1.25+
- GKE Workload Identity, or an existing Kubernetes ServiceAccount with equivalent identity
- One GCS bucket and immutable world ID per world; an empty bucket is created on first start
- One existing multi-SAN TLS Secret, or cert-manager and a suitable Issuer

The chart never creates buckets, PVCs, or TLS Secrets. The optional
`Certificate` asks cert-manager to populate the referenced TLS Secret.

Token Secrets are runtime-owned (the broker appends minted hashes to
`tokens.toml`), so the chart never templates them either. They live in the
release namespace, since the server pod mounts them: point the broker's
`worldDefaults.namespace` here. Each world mounts
two optional Secrets: `tokenSecret` (default `<name>-tokens`), which the
broker creates on its first mint, and `staticTokenSecret` (default
`<name>-static-tokens`), which holds entries you own and the broker never
touches. A world opens with either or both absent and reloads as they are
projected. By default a pre-install/pre-upgrade bootstrap Job
(`tokens.bootstrap`) seeds any missing `tokenSecret` with a publish-only
admin entry and hands off; existing Secrets are left untouched. When it
creates one and `tokens.emitRawValues: true`, it also writes the raw token
to a `<world>-token-values` Secret, which the demarkus-agent chart consumes
directly through `tokens.fromWorldSecrets`. A world whose `tokenSecret`
already existed is skipped entirely, raw Secret included: supply the agent's
token through `staticTokenSecret` and a raw Secret of your own, as in the
GitOps section, before enabling the agent. The Job's `tokens.bootstrap.image`
defaults to kubectl 1.35, which supports API servers 1.34–1.36; override it
to match older clusters (kubectl's skew policy is ±1 minor). For a GitOps
install, disable the Job; see [GitOps install](#gitops-install-without-the-bootstrap-job).

Buckets need no out-of-band initialization. A world whose bucket is empty is
created on first start: the server writes the world skeleton and seeds a
default write policy that warns rather than blocks, then enforces it. Publish
your own policy to `/.well-known/demarkus/policy.md` through the protocol and
it governs the next write; restarts never revert it. A bucket holding objects
but no world head is refused, so a mistyped `bucket.url` naming a bucket
already in use fails the open. A typo naming some other empty bucket still
creates a world there, logged at Warn.

To choose that first policy instead of taking the default, set
`initialPolicy` on a world (or on `worldDefaults` for every world). The body
ships as a key in the config ConfigMap, is projected next to `config.yaml`,
and the server seeds it as version 1. Seeding is create-only: a world that
already holds a policy keeps it, so the chart never overwrites or verifies a
policy in a bucket. The projected file is still read and validated on every
world open, so leave it in place once a world has started with it. A read-only
world is never seeded, and setting `initialPolicy` on one fails rendering.

Policy directives parse anywhere in the body, so prose in `initialPolicy`
must never begin a line with `strictness:`, `require_tags:`, or
`require_fields:`.

```yaml
worlds:
  - name: team-a
    initialPolicy: |
      # Write Policy

      Tag every publish with a category axis.

      strictness: block
      require_tags: category
```

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

worldDefaults:
  bucketPrefix: gs://deployment-

worlds:
  - name: team-a
    worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
  - name: team-b
    worldID: 42b471f7-8d38-4c89-b44a-6f4f8b1a4f49
    authorities:
      - team-b.example.com
    bucket:
      url: gs://other-bucket
```

Only `name` and `worldID` are required. Each world derives
`authorities: [<name>.<authorityDomain>]`, `bucket.url: <bucketPrefix><name>`,
`tokenSecret: {name: <name>-tokens, key: tokens.toml}` and
`staticTokenSecret: {name: <name>-static-tokens, key: tokens.toml}`; a field
set on the world wins. `worldDefaults.authorityDomain` defaults to
`<fullname>.<namespace>.svc.cluster.local`, the same suffix the broker and
agent charts derive, so the three agree without ExternalName aliases.
Under the `demarkus-knowledge-system` umbrella the list and the defaults
come from `global.worlds`, `global.bucketPrefix` and `global.authorityDomain`.

`tls.certManager.selfSigned.create: true` renders a namespaced self-signed
`Issuer` and points the Certificate at it, for clusters without a CA issuer;
clients then dial with verification off.

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
Rotating token values within a world hot-reloads through the file watch or
`SIGHUP`; moving a token hash from one world to another is rejected at reload
and requires a restart with the new assignment.

## GitOps install without the bootstrap Job

Argo CD and Flux render with `helm template`, so the bootstrap Job is the one
imperative step in the chart, with its own image (an allowlist entry under
Binary Authorization), RBAC and hook ordering. Drop it:

```yaml
tokens:
  bootstrap:
    enabled: false
```

Every world then starts with no token Secret at all, serves reads, and
accepts writes as soon as a Secret lands; no restart, no `SIGHUP`:

- `<name>-tokens` is the broker's. It creates the Secret on its first mint
  and appends afterwards. Never template it: a reconciler would reset the
  broker's entries on every sync.
- `<name>-static-tokens` is yours: entries the broker never touches. It
  holds hashes only, so a plain Secret manifest in git is fine.

The agent's publish token for the hub world needs nothing from you when the
demarkus-knowledge-broker chart runs alongside: its `agentTokens` (derived
for every `hub: true` world) has the broker mint the token, add the hash to
`<hub>-tokens` and write the raw value to `<hub>-token-values`, which the
agent chart projects by default (`tokens.fromWorldSecrets`). The broker
leaves a `<hub>-token-values` it did not create alone.

Without the broker, mint the token yourself. The hash goes into the static
Secret, the raw value into `<hub>-token-values`. Keep the raw Secret out of
git (an ExternalSecret, a SealedSecret, or SOPS):

```bash
TOKEN=$(openssl rand -hex 32)
HASH="sha256-$(printf %s "$TOKEN" | shasum -a 256 | cut -d' ' -f1)"
cat > root-static-tokens.yaml <<MANIFEST
apiVersion: v1
kind: Secret
metadata:
  name: root-static-tokens
stringData:
  tokens.toml: |
    [tokens.admin]
    hash = "$HASH"
    paths = ["/**"]
    operations = ["publish"]
MANIFEST
kubectl -n demarkus apply -f root-static-tokens.yaml   # or commit it for the reconciler
kubectl -n demarkus create secret generic root-token-values --from-literal=admin="$TOKEN"
```

The static Secret must reach the cluster before the agent publishes, either
applied by hand as above or committed next to the Application so the
reconciler creates it.

Keep static entries publish-only: any `read` operation flips the world into
read-auth mode and breaks the broker's open reads. A hash may appear in only
one world and only one file; the world refuses the load otherwise. Two
syncs in a row leave `<name>-tokens` as the broker wrote it, because nothing
in the rendered manifests names it.

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

Each world's token Secrets are projected read-only into a distinct path. TLS
and configuration are also read-only. Pods run as UID/GID 65532 with a read-only
root filesystem, dropped capabilities, RuntimeDefault seccomp, node and zone
topology spread, rolling updates, and a default PDB.

## Validation

Template rendering fails for missing TLS, Workload Identity, world name or
ID, or a bucket that neither the world nor a prefix supplies. It also rejects fewer than two
replicas, an `initialPolicy` on a read-only world, and duplicate world names,
normalized authorities, buckets, world IDs, or token Secret names (runtime
and static together).

```sh
helm lint ./deploy/helm/demarkus-knowledge-server --values production.yaml
helm unittest ./deploy/helm/demarkus-knowledge-server
helm template knowledge ./deploy/helm/demarkus-knowledge-server \
  --namespace demarkus-knowledge --values production.yaml
```

## Resource names

Resources are named after the release (`fullnameOverride` still wins). A
release whose name did not contain the chart name and set no override is
renamed on upgrade; set `fullnameOverride` to the old fullname to keep it.
