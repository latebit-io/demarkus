# demarkus-server-common library chart

Shared templates for the [demarkus-server](../demarkus-server) and [demarkus-server-pg](../demarkus-server-pg) charts. It renders nothing on its own (`type: library`); the two backend charts vendor it through a `file://` dependency so a fix to a probe, a security context, or the token bootstrap Job cannot land in one chart and miss the other.

## What lives here

Everything that does not depend on the storage backend: the StatefulSet skeleton, Service, cert-manager Certificate, ServiceAccount, token-bootstrap Job, and the name/label helpers.

## What a backend chart must supply

The StatefulSet skeleton includes five templates that each backend chart defines in its own `templates/_backend.tpl`. Helm fails the render if one is missing, so a new backend cannot forget a piece:

| Template | Purpose | Empty allowed |
|---|---|---|
| `demarkus-server.backendArgs` | store flags on the server command line | no |
| `demarkus-server.backendEnv` | extra container env, e.g. a DSN from a Secret | yes |
| `demarkus-server.backendVolumeMounts` | mounts the backend needs | yes |
| `demarkus-server.backendVolumeClaims` | `volumeClaimTemplates` entries | yes |
| `demarkus-server.backendGuard` | required-value checks and the refusal to upgrade across backends; renders nothing | yes |

Keeping the postgres specifics out of this chart is deliberate: deleting the Postgres backend means deleting the `demarkus-server-pg` directory, with nothing to unpick here.

## Development

The dependency is vendored into each consuming chart at package time:

```bash
helm dependency update deploy/helm/demarkus-server
helm dependency update deploy/helm/demarkus-server-pg
```

`deploy/helm/*/charts/` and `Chart.lock` are build artifacts and are gitignored; CI and the release workflow run the command above before lint, unittest, and package.
