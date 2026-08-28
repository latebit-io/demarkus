# Kubernetes & Helm

This page covers deploying Demarkus on Kubernetes with the Helm charts in `deploy/helm/`. Use it for multi-replica production deployments; for a single host, see [Deployment & TLS](index.md) or the [appliance](appliance.md).

## Charts

| Chart | Kind | Purpose |
|---|---|---|
| `demarkus-server` | StatefulSet | Single world over the filesystem store (PVC) |
| `demarkus-server-common` | library | Shared backend-neutral server templates |
| `demarkus-knowledge-broker` | Deployment | OIDC broker and MCP-over-HTTPS gateway (defaults to 2 replicas + PDB) |
| `demarkus-agent` | Deployment | Federation crawler: scheduled crawl, hash indexes, graph snapshots |
| `demarkus-knowledge-server` | Deployment | Multi-world production server, GCS-backed |

Each chart's `README.md` under `deploy/helm/<chart>/` documents every value; this page is the map.

## Knowledge server (`demarkus-knowledge-server`)

One deployment serves every world on one UDP listener; TLS SNI selects the world during the QUIC handshake, and each world keeps its own GCS bucket, tokens, policy, and rate limits.

Prerequisites per world:

- A GCS bucket with an immutable world ID (a canonical RFC 4122 UUID).
- The bucket initialized and its publish policy seeded with `demarkus-knowledge-bootstrap` (`-bucket gs://... -world-id <uuid> -policy-file policy.md`); the bootstrap binary ships in the `demarkus-knowledge-server` release archive.
- A token Secret referenced by `worlds[].tokenSecret`.

Cluster prerequisites:

- TLS: either `tls.existingSecret` or `tls.certManager` (mutually exclusive, one required); the certificate must cover every world authority.
- GCS credentials via Workload Identity: `serviceAccount.workloadIdentity.gsa` binds the pod to a Google service account; there are no credential env vars.
- Egress: NetworkPolicy cannot express GCS hostnames, so the chart refuses to render unless `networkPolicy.allowUnrestrictedHTTPS: true` is set (or the policy is disabled).

High availability:

- Replicas are stateless; all durable state lives in the buckets.
- Writes race on a compare-and-swap of one head object per world; there is no leader election and no lock.
- The chart rejects `replicaCount` below 2 and ships a PodDisruptionBudget and zone spread constraints by default.

Minimal values sketch:

```yaml
replicaCount: 2
worlds:
  - name: acme
    authorities: ["acme.knowledge.example.com"]
    bucket:
      url: gs://acme-world
      worldID: 6f1c9c2e-6e2a-4b7d-9f1e-3f2a1b4c5d6e
    tokenSecret:
      name: acme-world-tokens
      key: tokens.toml
tls:
  certManager:
    enabled: true
    issuerRef: {kind: ClusterIssuer, name: letsencrypt}
serviceAccount:
  workloadIdentity:
    gsa: demarkus-knowledge@project.iam.gserviceaccount.com
networkPolicy:
  allowUnrestrictedHTTPS: true
```

See `deploy/helm/demarkus-knowledge-server/README.md` for the full surface (limits, world defaults, probes, service, spread).

## Broker and agent

- `demarkus-knowledge-broker`: OIDC issuer config, per-world routing, and the MCP gateway that agents join with `/knowledge-join`; defaults to 2 replicas with a Lease-based leader election for the token sweeper only.
- `demarkus-agent`: crawl seeds, hubs, schedule, and per-authority endpoint overrides (`config.endpoints`).

## Dev harness (kind)

`deploy/kind/up.sh` stands up a local cluster with the server chart, optionally the broker with a mock OIDC issuer (`--with-broker`), ArgoCD ApplicationSets (`--with-argo`), and an MCP auth smoke test (`--with-mcp-smoke`). It does not cover the knowledge server; that path requires real GCS.

## Related

- [Deployment & TLS](index.md)
- [Run a Server](../server/index.md)
- [Configuration Reference](../reference/index.md)
- [Security Model](../security/index.md)
