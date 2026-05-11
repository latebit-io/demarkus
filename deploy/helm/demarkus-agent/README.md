# demarkus-agent Helm chart

Deploys [demarkus-agent](https://github.com/latebit-io/demarkus) as a federation crawler in Kubernetes. The agent crawls a list of seed Mark Protocol servers, collects content hashes, and publishes aggregated (or per-server) indexes to one or more hubs on a schedule.

## Quick start

```bash
helm install agent ./deploy/helm/demarkus-agent \
  --set 'config.seeds[0]=mark://team-a.example.com:6309' \
  --set 'config.seeds[1]=mark://team-b.example.com:6309' \
  --set 'config.hubs[0]=mark://hub.example.com:6309' \
  --set 'tokens.inline.hub\.example\.com:6309=<write-token>' \
  --set config.schedule.interval=15m
```

For private seeds (read-auth enabled), add a token entry for that host as well.

## Values

| Key | Type | Default | Description |
|---|---|---|---|
| `image.repository` | string | `ghcr.io/latebit-io/demarkus-agent` | Container image |
| `image.tag` | string | `""` (uses `.Chart.AppVersion`) | Image tag |
| `image.pullPolicy` | string | `IfNotPresent` | |
| `replicaCount` | int | `1` | Keep at 1 — multiple replicas re-do the same crawl |
| `config.seeds` | list | `[]` | `mark://` URLs to crawl. **Required.** |
| `config.hubs` | list | `[]` | `mark://` URLs to publish indexes to. Empty = crawl-only mode |
| `config.crawl.maxDepth` | int | `2` | Max link hops per server |
| `config.crawl.maxServers` | int | `50` | Max servers to discover per run |
| `config.crawl.maxDocuments` | int | `1000` | Max documents per crawl run |
| `config.crawl.workers` | int | `5` | Concurrent fetch goroutines |
| `config.schedule.interval` | duration | `6h` | Time between crawl runs |
| `config.politeness.requestDelay` | duration | `100ms` | Delay between requests to same server |
| `config.politeness.perServerConcurrency` | int | `2` | Max concurrent requests per server |
| `perServer` | bool | `false` | `true` → one index per server at `/index/<host>.md`; `false` → single aggregated `/index.md` |
| `insecure` | bool | `false` | Skip TLS cert verification (self-signed dev clusters only) |
| `tokens.existingSecret` | string | `""` | Name of an existing Secret with key `tokens.toml`. Takes precedence over `tokens.inline` |
| `tokens.inline` | map | `{}` | Inline `host:port → token` map. Generates `<release>-tokens` Secret |
| `state.persistent` | bool | `false` | If true, mount a PVC for the crawl-politeness state file |
| `state.storageClassName` | string | `""` | StorageClass for the state PVC |
| `state.size` | string | `1Gi` | PVC size |
| `resources` | object | `50m/64Mi → 500m/256Mi` | Resource requests/limits |
| `serviceAccount.create` | bool | `true` | |
| `serviceAccount.name` | string | `""` (auto) | |
| `serviceAccount.workloadIdentity.gsa` | string | `""` | GKE Workload Identity GSA binding |
| `podSecurityContext` | object | non-root user 65532 | Pod-level security |
| `securityContext` | object | read-only root FS, drop all caps | Container-level security |

## How auth is wired

The agent uses the protocol-client tokens convention. On startup it calls `tokens.LoadDefault()` which reads `~/.mark/tokens.toml`. The chart mounts the tokens Secret at `/home/demarkus/.mark/tokens.toml` and sets `HOME=/home/demarkus`, so the existing code path Just Works.

For a single hub with a single token, you can also set the `DEMARKUS_AUTH` env var via `podAnnotations` or by patching the Deployment — but the file path is preferred for multi-host setups.

## How publishing modes interact

| Setting | What the agent publishes |
|---|---|
| `hubs: []` | Nothing. Crawl-only. State is updated. |
| `hubs: [X]`, `perServer: false` | Single aggregated `/index.md` to each hub |
| `hubs: [X]`, `perServer: true` | One `/index/<server>.md` per discovered seed, to each hub |

Re-publishing is idempotent: the agent uses `expected_version=-1` (no check), and the server's no-op-on-duplicate-content prevents version churn when the computed index is unchanged.

## Probes

Intentionally omitted. The agent is an outbound scheduled job, not a service. Kubernetes restarts on process exit — the daemon loop logs and continues on transient crawl errors and exits only on configuration / unrecoverable failures. An exec probe would only verify the binary, not crawl health, so it's misleading.

## Persistence

The agent's state file holds per-URL `etag` and last-seen timestamps for crawl politeness (conditional FETCH). It is not authoritative content — losing it just means a fresh crawl on the next tick, no data loss on hubs. Default is ephemeral (`emptyDir`). Set `state.persistent: true` only if you crawl very large universes where the etag cache is worth preserving across restarts.

## Image

The image `ghcr.io/latebit-io/demarkus-agent` is published as part of the release pipeline (see Phase 6.7 in `/plans/universe-deployment.md`). For local development, build with:

```bash
cd client && go build -o ../demarkus-agent ./cmd/demarkus-agent
# Build a container image with that binary as ENTRYPOINT
```

## Tests

Helm unit tests are in `tests/`. Install [helm-unittest](https://github.com/helm-unittest/helm-unittest) and run:

```bash
helm unittest deploy/helm/demarkus-agent/
```

## See also

- `/plans/universe-deployment.md` — full Phase 6 plan
- `/architecture.md#agent-driven-hash-discovery` — federation pattern
- `client/cmd/demarkus-agent/main.go` — agent CLI
- `client/internal/fedcrawl/` — crawl + index + publish core
