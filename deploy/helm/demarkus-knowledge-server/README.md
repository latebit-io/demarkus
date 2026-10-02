# demarkus-knowledge-server

Production Helm chart for `demarkus-knowledge`: the multi-world knowledge
server and the OIDC broker with its MCP gateways in one process. One
Deployment serves every configured authority over a shared UDP Service,
stores each world in its own pre-provisioned GCS bucket, and fronts the
worlds with an HTTP listener (login, device flow, `/me/install`, and the
knowledge and memory MCP gateways at `/mcp`) behind an Ingress. Tool calls
reach the worlds in process; an identity a world's `allow` admits writes
under its `writeScope` with no token minted. See
[Broker and MCP gateways](#broker-and-mcp-gateways).

## Prerequisites

- Kubernetes 1.25+
- GKE Workload Identity, or an existing Kubernetes ServiceAccount with equivalent identity
- One GCS bucket and immutable world ID per world; an empty bucket is created on first start
- One existing multi-SAN TLS Secret for QUIC, or cert-manager and a suitable Issuer
- An OIDC client registered at your IdP (`broker.oidc`) and the broker's public URL
- One dedicated GCS bucket for broker state (`broker.stateBucket`), with
  `roles/storage.objectAdmin` for the workload identity

The chart never creates buckets, PVCs, or TLS Secrets. The optional
`Certificate` asks cert-manager to populate the referenced TLS Secret.

Token Secrets are runtime-owned (the broker appends the agent token hash it
mints to `tokens.toml`), so the chart never templates them either. They live
in the release namespace, since the pod mounts them. Each world mounts two
optional Secrets: `tokenSecret` (default `<name>-tokens`), which the broker
creates on its first agent token mint, and `staticTokenSecret` (default
`<name>-static-tokens`), which holds entries you own and the broker never
touches. Identities writing through the gateway need neither. A world opens with either or both absent and reloads as they are
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
    allow:
      groups: ["team-a"]
  - name: team-b
    worldID: 42b471f7-8d38-4c89-b44a-6f4f8b1a4f49
    authorities:
      - team-b.example.com
    bucket:
      url: gs://other-bucket

broker:
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
```

Only `name` and `worldID` are required. Each world derives
`authorities: [<name>.<authorityDomain>]`, `bucket.url: <bucketPrefix><name>`,
`tokenSecret: {name: <name>-tokens, key: tokens.toml}`,
`staticTokenSecret: {name: <name>-static-tokens, key: tokens.toml}` and
`profile: knowledge`; `allow` and `writeScope` come from `worldDefaults`
(everyone admitted, `/**`); a field set on the world wins.
`worldDefaults.authorityDomain` defaults to
`<fullname>.<namespace>.svc.cluster.local`, the same suffix the agent chart
derives, so the two agree without ExternalName aliases; it is also the SNI
the gateway routes each world by. Under the `demarkus-knowledge-system`
umbrella the list and the defaults come from `global.worlds`,
`global.bucketPrefix` and `global.authorityDomain`.

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

- `<name>-tokens` is the broker's. It creates the Secret on its first agent
  token mint and appends afterwards. Never template it: a reconciler would
  reset the broker's entries on every sync.
- `<name>-static-tokens` is yours: entries the broker never touches. It
  holds hashes only, so a plain Secret manifest in git is fine.

The agent's publish token for the hub world needs nothing from you:
`broker.agentTokens` (derived for every `hub: true` world) has the broker
mint the token, add the hash to `<hub>-tokens` and write the raw value to
`<hub>-token-values`, which the agent chart projects by default
(`tokens.fromWorldSecrets`). The broker leaves a `<hub>-token-values` it did
not create alone.

To mint the token yourself instead (`broker.agentTokens: []`): The hash goes into the static
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

## Broker and MCP gateways

The process listens three times: UDP `server.udpPort` for QUIC (direct
clients with capability tokens, the agent, other clusters), UDP
`server.bearerPort` for QUIC clients signed in at the IdP (below), and TCP
`server.httpPort` for plain HTTP behind the Ingress. One mux serves the management API (OIDC
login, RFC 8628 device flow, RFC 7591 registration, `/me/install`, the
discovery documents) and both MCP gateways; the gateway a request reaches
is chosen by its `Host`: `broker.memory.publicURL`'s hostname selects the
memory profile, every other host the knowledge profile. The Ingress may
carry one hostname or three (`ingress.host`, `ingress.mcp.host`,
`ingress.memory.host`); routes never overlap.

```yaml
broker:
  publicURL: https://broker.example.com          # issuer, management host
  mcp:
    publicURL: https://mcp.example.com           # optional own hostname
  memory:
    publicURL: https://memory.example.com        # turns the memory gateway on
ingress:
  enabled: true
  host: broker.example.com
  mcp:
    host: mcp.example.com
  memory:
    host: memory.example.com
  tls:
    certManager:
      enabled: true                              # one Certificate, every host
```

Plugins join with `/knowledge-join <knowledge gateway URL>`; hosts add
`<publicURL>/mcp` as a remote MCP server and walk the OAuth flow (RFC 9728
resource metadata on the gateway, RFC 8414 on the issuer host). A client
that sends an RFC 8707 `resource` gets a token bound to that gateway; one
that does not gets a token valid at both. The tool contract is in
`knowledge/cmd/demarkus-knowledge/MCP-API.md` and `MEMORY-API.md`.

Access model: reads are open to any verified identity the world's `allow`
admits; writes run under that identity's grant on `writeScope.paths`, in
process, with no token. Static worlds carry `profile: knowledge`; a
`profile: memory` world names its single tenant in `allow`. Worlds served
in another cluster go under `broker.remoteWorlds` and are read over QUIC
(`broker.worldDialer`), never written.

### Bearer listener

`server.bearerPort` (8443 in the pod, `service.bearerPort` 443 on the
Service) serves the mark protocol to clients that hold an IdP or broker
token instead of a capability token: the token goes in the request's `auth`
field, and the broker admits each request by the same rules as the
gateways (ADR 0033 on the soul). Every verb needs a token; reads pass the
org gate, writes need the world's `allow` and land in its `writeScope`, a
memory world admits its tenant alone, and a token bound to one gateway
works on that gateway's worlds. A watch ends with `unauthorized` when its
token expires; the client resumes from the cursor with a fresh one. The
connection's SNI is the world's authority, as on 6309, so the certificate
and DNS are the ones the worlds already have:

```bash
demarkus -auth "$TOKEN" mark://team-a.example.com:443/index.md
```

Paths guarded by a `read` capability token stay closed on this listener.

Provisioning (`provisioning.mode: allowlisted | open`) creates a memory
tenant on first arrival: bucket `gs://<bucketPrefix><slug>` in
`provisioning.bucketProject`, a registry entry, and a worlds fragment in
`provisioning.worldsSecret` the pods mount for restarts. The tenant is served
in this process at once; sibling replicas converge through the registry.
The pod needs GCP credentials with bucket-create rights (Workload Identity
and `roles/storage.admin` on the project, or narrower with a bucket name
condition on the prefix). Deprovisioning is explicit:

```bash
kubectl -n demarkus exec deploy/knowledge -- \
  /demarkus-knowledge -broker-config /etc/demarkus/broker/config.yaml \
  -deprovision-tenant <world> [-delete-bucket]
```

It tombstones the registry entry, rewrites the fragment (every replica drops
the world), optionally destroys the bucket, then clears the tombstone; a
crashed run converges on rerun.

Rate limits: `broker.rateLimit` buckets `/me/install` and the gateways per
subject and `/auth/login` per IP, per replica. `trustForwardedFor` is on by
default and is only safe behind an Ingress controller that strips spoofed
`X-Forwarded-For` (nginx-ingress and Traefik do); turn it off without one.

Logins and MCP host registrations live in `broker.stateBucket`, one object
each: `refresh/<user>/<login>` and `clients/<client_id>`. The user key is a
hash of the identity, and a record holds claims and secret hashes, never a
token. Never point it at a world's bucket. A public client's refresh token
rotates on every refresh; a replaced token works for one more minute, then
presenting it revokes the login. A `webClients` login keeps its token, since
the client authenticates on every refresh. `broker.maxSessionsPerUser` (default 20) caps one user's
logins, revoking the oldest. The leader-elected sweeper deletes logins past
their 90 days. An optional lifecycle rule deleting objects older than 91
days is a safe backstop: a refresh rewrites the object, so it only catches
records already expired.
Keys live in Secrets the broker creates on first start (`<fullname>-signing-key`,
`<fullname>-cookie-key`). The signed-state cookie key never comes from the
render, so `helm template` is deterministic; own it through
`broker.existingCookieKeyRef`. Logins in
flight live in `<fullname>-oauth-state` (`broker.oauthStateSecret`), which
the broker creates on first use: issued authorization codes and device
grants under the hash of each code, holding claims only, swept as they
expire (60 seconds and ten minutes) and capped, so a login may cross
replicas. Uninstall leaves it behind; it holds nothing past ten minutes. Rotate the
signing key by deleting its Secret and restarting every replica; in-flight
broker-signed tokens are invalidated. The graph store behind
`mark_backlinks`, `mark_graph` and friends is process memory and rebuilds
after a restart.

Federation: with `broker.federation.hub` set, one replica (Lease
`broker.federation.leaseName`) derives each static knowledge world's graph
from its change feed and checkpoints it into the hub's
`/graph/worlds/<world>/`, from which the gateway seeds those worlds. No token
or Secret is involved: writes run in process under a grant to
`/graph/worlds/**`, and reads are anonymous, so no read token may cover that
path in the hub. A changed world checkpoints after `quietPeriod` (default
30s), at most `interval` (default 1m) after its first change, and never
sooner than `interval` after its last checkpoint. A checkpoint writes each
changed shard plus the manifest, one commit each at the bucket store's pace
of one per 1.5 s per world: an edit costs the hub about 3 s of write
capacity, a first build at most about 25 s per world.

Production checklist: `broker.oidc.existingSecretRef` instead of a cleartext
`clientSecret` (it lives in helm release history); Ingress TLS from
cert-manager or an existing Secret; verify the `X-Forwarded-For` invariant
for your controller; keep NetworkPolicy on with `ingressFromNamespace` set
to the controller's namespace; size `resources` for the sum of worlds and
gateway sessions.

## Security

Health endpoints listen on the private pod port only. No health Service is
created. NetworkPolicy admits the HTTP listener from the Ingress controller
namespace only, UDP ingress on the QUIC port only from the configured agent
namespace and pod selector and optional `externalCIDRs`, and the bearer port
from any source, since every request there carries a verified token. A
LoadBalancer remains blocked from capability token clients until those CIDRs
are set. Egress permits cluster DNS,
TCP 443 (GCS, the OIDC issuer, the API server), GKE metadata-server
endpoints, and the UDP ports of `broker.remoteWorlds`.

GCS egress cannot be restricted to stable CIDRs with standard Kubernetes
NetworkPolicy. The built-in policy therefore requires explicit
`allowUnrestrictedHTTPS: true`. Otherwise disable it and provide an egress proxy
or CNI FQDN policy.

The broker's Role is scoped to its own state Secrets, the sweeper Lease (and
the federation Lease when `broker.federation.hub` is set), the
agent token records and the tokens Secrets of hub worlds, plus the registry
and worlds fragment when provisioning is on; `create` is namespace-wide
because RBAC cannot name a Secret before it exists. Internet-facing OAuth
and the store share one pod: the mitigation is the hardened HTTP server and
the pod security context, not a process boundary.

Each world's token Secrets are projected read-only into a distinct path. TLS
and configuration are also read-only. Pods run as UID/GID 65532 with a read-only
root filesystem, dropped capabilities, RuntimeDefault seccomp, node and zone
topology spread, rolling updates, and a default PDB.

## Validation

Template rendering fails for missing TLS, Workload Identity, world name or
ID, or a bucket that neither the world nor a prefix supplies. It also rejects fewer than two
replicas, an `initialPolicy` on a read-only world, and duplicate world names,
normalized authorities, buckets, world IDs, or token Secret names (runtime
and static together). The broker half fails on a missing `publicURL` or IdP
registration, two OIDC client secret modes, a memory gateway on the
knowledge gateway's host, a memory world without a tenant, provisioning
without the memory gateway, a bucket prefix or a tenant cap in open mode,
and an Ingress without a host.

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
The broker's Secrets change name with it: a new signing key invalidates
broker-signed tokens in flight. Point `broker.signingKeySecret` at the old
name to keep it; the tenant registry keeps the binary's default name.

Upgrading to `broker.stateBucket`: create the bucket and grant the workload
identity first. At start the broker imports MCP host registrations from
`<fullname>-dynamic-clients` and empties it. Refresh tokens are not imported,
so every user logs in once. Delete `<fullname>-refresh-tokens` after the
rollout, and `<fullname>-dynamic-clients` once every replica has started.
