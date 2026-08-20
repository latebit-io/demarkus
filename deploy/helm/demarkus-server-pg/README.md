# demarkus-server-pg Helm chart

Deploys one [demarkus](https://github.com/latebit-io/demarkus) world backed by Postgres. Same server as the [demarkus-server](../demarkus-server) chart, built with the pgstore backend linked in: documents, versions and the LOOKUP catalog all live in Postgres, so the pod is stateless and no content PersistentVolume is created.

Use this chart when you want multiple writers or replicas against one world. Use `demarkus-server` when you want the original single-binary deployment with no external service.

## Quick start

```bash
helm install team-a ./deploy/helm/demarkus-server-pg \
  --set server.postgres.dsnSecret.name=demarkus-pg-app
```

`server.postgres.dsnSecret` names a Secret holding the connection string; the chart refuses to render without it. The DSN reaches the pod through `secretKeyRef`, so credentials never appear in a values file or the pod spec. With [CloudNativePG](https://cloudnative-pg.io) the cluster's `<cluster>-app` Secret already carries a full DSN under the key `uri`, which is this chart's default key.

The server applies its schema at startup, including the `pg_trgm` extension used by the LOOKUP indexes, so the role in the DSN needs rights to create extensions and tables in its schema on first boot.

## Differences from the demarkus-server chart

| | demarkus-server | demarkus-server-pg |
|---|---|---|
| Image | `demarkus-server` (no database driver) | `demarkus-server-pg` (also carries `demarkus-migrate`) |
| Content | PVC per replica, via `volumeClaimTemplates` | Postgres; no PVC, no `storage` values |
| Extra values | `storage.*` | `server.postgres.dsnSecret.*` |
| Replicas | one writer | safe with several |

Everything else — service, TLS and cert-manager wiring, token bootstrap, probes, security contexts, resources — is identical and comes from the shared `demarkus-server-common` library chart. See the [demarkus-server README](../demarkus-server/README.md) for those settings.

## Moving an existing world onto Postgres

The charts deliberately refuse to swap a live release from one backend to the other: `volumeClaimTemplates` is immutable and no data would move on its own. The procedure is explicit:

1. Stop writes to the file-store release.
2. Copy the world with `demarkus-migrate`, which is bundled in this chart's image: `demarkus-migrate -from file -to postgres -root /var/lib/demarkus/content -pg-dsn "$DSN"`. It refuses a non-empty destination and verifies every stored version after the copy.
3. `helm uninstall` the `demarkus-server` release and delete its retained content PVC.
4. `helm install` this chart under the same release name.

Rendering this chart onto a release that still has a content PVC fails with that procedure in the error message, rather than letting Kubernetes reject the update halfway.

## Development

Both server charts vendor the shared library chart, so build dependencies before rendering or linting:

```bash
helm dependency update deploy/helm/demarkus-server-pg
helm unittest deploy/helm/demarkus-server-pg
```
