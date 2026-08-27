# ADR 0006: The Postgres backend is an optional build, not a dependency

Status: superseded by [ADR 0010](0010-remove-postgres-backend.md) (2026-08-27).
Originally accepted 2026-08-20, implemented on the same branch as the step 8
performance work.

## Context

demarkus started with a deliberate constraint: the server has no external
service dependencies. Content is files, the index is in memory, and a
deployment is one binary and a directory. The Postgres backend added multi
writer safety and a DB-backed LOOKUP, but it also put `github.com/jackc/pgx`
in `server/go.mod`, so every build of `demarkus-server` linked a database
driver that most deployments never call.

Two things needed to be true at once. A file-store deployment should carry no
database code at all, and the Postgres backend should be removable later
without unpicking the server, since it is still experimental.

## Decision

One codebase, two binaries and two charts, split at a single seam in each.

- `demarkus-server` (default build) links no database driver: `go tool nm`
  reports zero `jackc/pgx` symbols, which is what CI asserts. The binary is
  also materially smaller, though only the symbol check is enforced.
- `demarkus-server-pg` (`-tags pg`) links pgstore. `demarkus-migrate` is
  inherently Postgres and ships alongside it.
- The seam is a backend registry in `cmd/demarkus-server/store.go`: backends
  register an opener keyed by the `-store` value, and `main` just asks for
  the configured one. The file store registers unconditionally; `store_pg.go`
  (`//go:build pg`) registers Postgres from an `init()`. Nothing in the
  default build names Postgres at all, and the registry supplies both the
  `-store` help text and the "not available in this build (have: file)"
  error, so neither can claim a backend that was not compiled in. Store
  construction happens before the listener binds, so every backend fails
  before a port is taken, not after.
- Images follow the binaries: `server/Dockerfile.pg` produces
  `demarkus-server-pg`, bundling `/demarkus-migrate` so the backend flip
  procedure can run in cluster.
- Charts follow the images: `demarkus-server` (file store) and
  `demarkus-server-pg` are separate charts, each pinning its own image and
  carrying only its own values. Everything backend-neutral lives in the
  `demarkus-server-common` library chart, which both vendor through a
  `file://` dependency, so a probe or security-context fix cannot land in one
  chart and miss the other. The library knows nothing about Postgres: a
  backend chart supplies `backendArgs`, `backendEnv`, `backendVolumeMounts`,
  `backendVolumeClaims`, and `backendGuard`, and Helm fails the render if one
  is missing. The `server.store` value is gone; picking a backend is picking
  a chart, and each chart refuses to upgrade over a release running the other
  one.
- CI builds both flavors, vets the tagged one, and asserts that the default
  binary contains no pgx symbols, so the seam cannot leak unnoticed. It also
  diffs the two charts' non-backend values, because Helm never merges a
  library chart's values into its consumers: templates cannot drift, but
  defaults could.

## What was rejected

**Moving pgstore into its own Go module**, which is the only way to get pgx
out of `server/go.mod` entirely. It would break the parity invariant this
whole effort is built on. `handler/backend_test.go` is an internal test in
package `handler` that runs the entire handler suite against both backends
via `forEachBackend`; an internal test cannot import a package from another
module, so the Postgres half of that suite would have to be deleted or
rewritten as an external test elsewhere. Paying for module purity with the
proof that both backends behave identically is a bad trade.

The consequence is stated plainly: `server/go.mod` still requires pgx,
because the module still contains pgstore and its tests. What the tag buys is
binary purity, not module purity.

## Removing the backend later

The seam exists so this is a deletion, not a refactor:

1. `rm -r server/internal/pgstore server/cmd/demarkus-migrate`
2. `rm server/cmd/demarkus-server/store_pg.go server/Dockerfile.pg`: the
   registry needs no edit; one fewer backend simply registers.
3. `rm -r deploy/helm/demarkus-server-pg`, and fold the library chart back
   into `demarkus-server` if a single backend no longer justifies it.
4. Drop `PostgresDSN` and the postgres branch from `internal/config`, and the
   `-pg-dsn` flag from `main.go`.
5. Delete the pg entries from `server/.goreleaser.yml`, the `server-pg` and
   `image-server-pg` Make targets, and the postgres steps in CI and the
   release workflow.
6. Delete the Postgres halves of the test suites: `pgstore/`, `pgtest/`,
   `postgresBackend` in `handler/backend_test.go`, and the pg factories in
   `storetest`. The suites themselves stay; they are the file store's
   contract too.
7. `cd server && go mod tidy`

## Consequences

- Anyone auditing the default binary finds no database driver, which is the
  claim the project makes about itself.
- Release archives split: `demarkus-server_*` and `demarkus-server-pg_*`, so
  a file-store operator never downloads the Postgres flavor.
- Two build paths must both keep compiling; CI enforces it, since a
  `-tags pg`-only compile error would otherwise reach a release. The same
  applies to the charts: CI lints and unit-tests both.
- Choosing a backend now means choosing a chart, so an existing pg
  deployment moves from `demarkus-server` to `demarkus-server-pg` rather
  than flipping a value. The resource names change with the chart name, so
  it is an uninstall and reinstall, which is what a backend change already
  required.
- The parity test suites live in the module, not behind the build tag, so
  they keep proving both backends. That holds wherever a Postgres DSN is
  configured: `DEMARKUS_TEST_PG_DSN` selects the database and CI sets
  `DEMARKUS_TEST_PG_REQUIRED=1` so a missing DSN fails the run instead of
  skipping the Postgres half.
