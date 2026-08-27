# ADR 0010: Remove the Postgres backend

Status: accepted (2026-08-27). Supersedes [ADR 0006](0006-postgres-backend-is-an-optional-build.md).

## Context

ADR 0006 made the Postgres backend an optional build behind the `-tags pg`
seam, explicitly so it could be removed later without unpicking the server.
Since then the knowledge server (ADR 0007, ADR 0008) became the deployment
for shared, multi-writer state: SNI-routed worlds over GCS with stateless
replicas. That covers the two things Postgres bought (multi-writer safety,
shared LOOKUP) without a database, and nobody deploys the pg flavor.

Keeping it costs real maintenance: a second binary, image, and chart in every
release, a Postgres service in CI, the parity suites' pg half, and pgx in
`server/go.mod`.

## Decision

Delete the backend along the ADR 0006 seam. `demarkus-server` is file-store
only; a database-backed store, if ever needed, belongs in the knowledge
server, not here.

Removed: `server/internal/pgstore` (with `pgtest`), `cmd/demarkus-migrate`,
`store_pg.go`, `Dockerfile.pg`, the `demarkus-server-pg` chart and kind
values, the e2e backend-parity script, the pg Make/goreleaser/CI/release
entries, `PostgresDSN` and the `-pg-dsn` flag, and the pg halves of the test
suites. `go mod tidy` drops pgx; CI's dependency-purity check on the default
binary stays (a package absent from `go list -deps` cannot be linked, so the
old symbol-level check went with it).

Kept: the backend registry in `cmd/demarkus-server/store.go` and the
`storetest` suites (conformance, LOOKUP, differential, migration round-trip).
They are the file store's contract, and `bucketstore` runs them too. The
`demarkus-server-common` library chart also stays: the backend seam in the
charts costs nothing and the knowledge-server chart may grow into it.

## Consequences

- One server binary, one server image, one server chart.
- `server/go.mod` carries no database driver at all; module purity, not just
  the binary purity ADR 0006 settled for.
- No migration path is needed: no pg deployments exist.
- `protocol/store.Migrator` stays: the file store implements it and the
  migration round-trip suite runs against it.
- Config no longer enumerates backend names; the store registry is the single
  authority and openStore rejects names not compiled in.
