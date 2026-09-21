# ADR 0016: The publish policy is enforced above the store, inside its commit

Status: accepted (2026-09-21). Amended by [ADR 0017](0017-archive-precondition-closed-views-and-seeding.md): seeding moved to `writepolicy.Seed` and the archive guard runs as a precondition.

## Context

Publish policy evaluation lived inside `bucketstore`, so the two backends were
not observably equivalent, conformance could not cover policy, and the store
carried a phase flag (`requirePolicy`) that `Open` flipped after seeding so
the seeding write would not be judged by the policy it created.

The obvious repair, a decorator above `backend.Store`, reads the policy in one
view and writes in a later commit. A write judged under policy P1 could then
land after a stricter P2. `bucketstore` does not have that window today: it
reads the policy inside each commit attempt and judges the write again after
every rebase.

## Decision

- `backend.WriteRequest` carries an optional `Precondition`. A backend runs it
  inside its own commit, after the conflict, archive and no-op checks and
  before anything is stored, with a `PreparedWrite`: canonical path, final
  body (joined for APPEND), persisted metadata, and a `Reader` over the state
  the write commits against. Its error refuses the write and is returned as
  is. `filestore` runs it under its write lock through
  `protocol/store.WriteChecked`; `bucketstore` runs it in the mutation
  builder, so a rebased attempt is judged again.
- `server/internal/writepolicy` owns the policy logic once. `Enforce(store,
  Options{Require})` is a decorator that sets the precondition on PUBLISH and
  APPEND and refuses to archive a required policy document. `Validate` checks
  that a world holds a usable policy before a server starts enforcing.
  `PolicyError` and the policy sentinels live there.
- `bucketstore` enforces no policy. Its `RequirePolicy` option and the
  `requirePolicy` phase flag are gone; `PolicySeed` still seeds at open, and
  the write is ungated simply because the store is not yet decorated.
- The knowledge server wraps every world with `Enforce`, requiring a policy
  unless the world is provisioned by the broker. The file server is not
  wrapped: a file world that already holds a policy document would start
  refusing writes, which is a product decision, not a refactor.

## Consequences

- Conformance case `Precondition` and rejection conformance run on both
  backends; the rebase test that proves a changed policy is re-evaluated still
  passes, now through the decorator.
- A refused write on the file backend leaves no version and no per document
  directory behind.
- `MutationResult` no longer reports the strictness and result of a warn
  decision; nothing consumed them.
- The precondition is a general seam. Quotas are still inside `bucketstore`.
