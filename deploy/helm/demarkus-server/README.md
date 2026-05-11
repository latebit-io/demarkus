# demarkus-server Helm chart

Deploys one [demarkus](https://github.com/latebit-io/demarkus) world on Kubernetes — a Mark Protocol server serving versioned markdown over QUIC, with capability-based auth and TLS. One Helm release = one world.

## Quick start

```bash
helm install team-a ./deploy/helm/demarkus-server \
  --set storage.className=premium-rwo
```

That's enough to get a working world with:
- A `LoadBalancer` Service on UDP/6309 (cloud-provider provisions an external IP)
- A 5 GiB content PVC
- A self-signed dev TLS cert (good for testing; replace with cert-manager or a real Secret for production)
- An auto-generated `admin` token, preserved across upgrades

Read the admin token after install:

```bash
kubectl get secret team-a-demarkus-server-token-values \
  -o jsonpath='{.data.admin}' | base64 -d
```

## Production install (GKE example)

```bash
helm install team-a ./deploy/helm/demarkus-server \
  --set storage.className=premium-rwo \
  --set storage.size=20Gi \
  --set tls.certManager.enabled=true \
  --set 'tls.certManager.dnsNames[0]=team-a.library.example.com' \
  --set 'tls.certManager.issuerRef.name=letsencrypt-prod' \
  --set 'service.annotations.cloud\.google\.com/neg=\{"exposed_ports":\{"6309":\{\}\}\}'
```

## Values

### Image
| Key | Default | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/latebit-io/demarkus-server` | Must contain `demarkus-server`, `demarkus` (CLI for probes), and `demarkus-token` |
| `image.tag` | `""` (uses `.Chart.AppVersion`) | |
| `image.pullPolicy` | `IfNotPresent` | |

### Server
| Key | Default | Notes |
|---|---|---|
| `server.udpPort` | `6309` | Override to `443` for VPN/middlebox-hostile networks (Warp Zero Trust, etc.) |
| `server.readOnly` | `false` | When true, rejects all writes with `not-permitted` |
| `server.extraArgs` | `[]` | Appended to the server command line |

### Service
| Key | Default | Notes |
|---|---|---|
| `service.type` | `LoadBalancer` | `LoadBalancer` for prod; `ClusterIP`/`NodePort` for local dev |
| `service.loadBalancerIP` | `""` | Pin LB to a specific external IP (if your cloud supports it) |
| `service.annotations` | `{}` | E.g. `service.beta.kubernetes.io/aws-load-balancer-type: nlb` |

### Storage
| Key | Default | Notes |
|---|---|---|
| `storage.className` | `""` (cluster default) | Often `premium-rwo` (GKE), `gp3` (EKS), etc. |
| `storage.size` | `5Gi` | |
| `storage.accessMode` | `ReadWriteOnce` | Don't change — Phase 7 territory |

### TLS

Three modes — pick one:

1. **`tls.existingSecret`** — reference a pre-existing Secret with `tls.crt` and `tls.key` (made by cert-manager elsewhere, kustomize, or `kubectl create secret tls`).
2. **`tls.certManager.enabled: true`** — chart provisions a cert-manager `Certificate` resource targeting the configured `issuerRef`. Use **DNS-01** for issuers like Let's Encrypt (HTTP-01 cannot validate a QUIC-only service).
3. **Neither set** — the server auto-generates a self-signed dev cert at startup. Good for tests, not for production.

### Auth tokens

| Key | Default | Notes |
|---|---|---|
| `tokens.admin.label` | `admin` | TOML `[tokens.<label>]` |
| `tokens.admin.paths` | `["/*"]` | Path globs the token may operate on |
| `tokens.admin.operations` | `["publish", "read"]` | |
| `tokens.admin.token` | `""` (auto-generated) | Set to a literal raw token to skip auto-generation |
| `tokens.emitRawValues` | `true` | Emit the raw-token Secret for broker discovery |

**How it works.** The chart writes **two Secrets**:

- `<release>-tokens` — TOML `tokens.toml` with the SHA-256 *hash* of the admin token. Mounted into the server pod. The server never sees the raw token.
- `<release>-token-values` — the raw admin token, base64-encoded under key `<label>`. Annotated `demarkus.io/raw-tokens: "true"` so the future broker can discover and revoke. Server does not mount this Secret.

On `helm upgrade`, the chart uses `lookup` to read the existing `-token-values` and preserves the admin token — no churn, no surprise rotation.

### Probes
Liveness/readiness exec `demarkus -insecure -no-cache mark://localhost:<port>/.well-known/agent-manifest.md`. The manifest is always public per `/architecture.md`. Probe disabled root-fs writes via `-no-cache`. The image must include the `demarkus` CLI binary.

### Security defaults
Pod runs as non-root user 65532, with `seccompProfile: RuntimeDefault`. Container has read-only root filesystem, no privilege escalation, all capabilities dropped. `/tmp` and `/home/demarkus` are emptyDir for writable scratch space.

### Workload Identity (GKE)
Set `serviceAccount.workloadIdentity.gsa` to the GCP service account email. The chart annotates the ServiceAccount with `iam.gke.io/gcp-service-account: <gsa>`. Use `mergeOverwrite`, so this binding wins over any user-supplied annotations.

## Auth bootstrap walkthrough

The first administrator gets the token from the raw-values Secret and uses it to configure other clients (or as input to the broker):

```bash
# Read the admin token
TOKEN=$(kubectl get secret team-a-demarkus-server-token-values \
  -o jsonpath='{.data.admin}' | base64 -d)

# Use it from a laptop
demarkus -auth "$TOKEN" mark://team-a.library.example.com:6309/index.md
```

For production, the recommended path is:

1. Install the chart — get the bootstrap admin token from the Secret.
2. Install the broker (Phase 6.3) pointing at this world.
3. Use the broker to mint per-user tokens via OIDC.
4. Once verified, the bootstrap admin token can be revoked or rotated.

## Image requirement

`ghcr.io/latebit-io/demarkus-server` must be a multi-binary image containing:

- `/demarkus-server` (the server, ENTRYPOINT)
- `/demarkus` (CLI, used by exec probes)
- `/demarkus-token` (token mint utility, used by future broker integrations)

The current `server/Dockerfile` ships only `demarkus-server`. Building the multi-binary image is part of Phase 6.7 (release pipeline). For local testing in the meantime, build the image manually with all three binaries.

## Tests

helm-unittest suites live in `tests/`:

```bash
helm plugin install https://github.com/helm-unittest/helm-unittest
helm unittest deploy/helm/demarkus-server/
```

## See also

- `/plans/universe-deployment.md` — full Phase 6 plan
- `/architecture.md` — Mark Protocol architecture (capability auth, versioned store, content-addressed fetch)
- `deploy/helm/demarkus-agent/` — federation crawler chart (Phase 6.0)
