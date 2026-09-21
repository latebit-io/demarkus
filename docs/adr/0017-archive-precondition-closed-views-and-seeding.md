# ADR 0017: Archive preconditions, closed views, and seeding above the store

Status: accepted (2026-09-21). Amends ADR 0015 and ADR 0016.

## Context

Three edges of the backend contract were left uneven after ADR 0015 and 0016.

- `SetArchived` had no precondition seam, so the policy archive guard ran as a
  check before the store call, outside the commit that PUBLISH and APPEND use.
- A read on a closed view was unspecified: `bucketstore` answered
  `context.Canceled` and `filestore` kept reading, without the read lock its
  `Close` had just released.
- Policy seeding lived in `bucketstore` (`PolicySeed`, `ValidatePolicySeed`,
  `createPolicy`), so only one backend could be seeded and `knowledgeseed`
  imported a storage backend for a type.

## Decision

- `Store.SetArchived(ctx, ArchiveRequest{Path, Archived, Precondition})`. An
  `ArchivePrecondition` runs inside the backend's commit, after the not found
  and no-op checks and before anything is stored, with the canonical
  `storefmt.ArchiveChange` and a `Reader` over the state it commits against.
  `filestore` runs it under its write lock through
  `protocol/store.ArchiveChecked`; `bucketstore` runs it in the mutation
  builder, so a rebased attempt is judged again. `writepolicy.Enforce` refuses
  to archive a required policy through it, after the caller's own.
- Every read on a closed view answers `backend.ErrViewClosed`; `Close` is
  idempotent. This replaces the sentence in ADR 0015 that a read after `Close`
  fails as canceled.
- `writepolicy.Seed(ctx, store, PolicySeed)` seeds any `backend.Store`,
  create-only, and counts an archived policy as present. It runs on the store
  before `Enforce` wraps it, which is why the write is not judged by the policy
  it creates. `bucketstore.Options.PolicySeed` is gone. This replaces the
  sentence in ADR 0016 that `PolicySeed` still seeds at open.
- `bucketstore.Options.ReadOnly` is a store invariant: every mutation answers
  `backend.ErrReadOnly` before any I/O. The handler still refuses writes first,
  so nothing changes on the wire. A seed is a write like any other, so a
  read-only world without a policy fails to open instead of being skipped by a
  hand exception.
- The `bootstrap` world flag is deprecated and changes nothing at open: every
  writable world is seeded and must hold a usable policy (B6), through
  `writepolicy.Ensure`, which seeds and validates in one read. The embedded
  default seed carries `agent: demarkus-knowledge-server`
  (`publishpolicy.SeedAgent`); an operator's policy file does not. A
  broker publishes its own policy as version two only over a version one that
  carries that marker; any other policy is left alone, and a server that does
  not seed still gets the create-only publish. The key stays accepted, and
  brokers keep writing it, for one release because the fragment is parsed
  strictly; it still forbids `policy.file`.
- `handler.New(Config)` refuses a nil store or logger. The handler's fields are
  unexported and the three "not configured" branches are gone.

## Consequences

- Conformance cases `ArchivePrecondition` and `ClosedView` run on both backends.
- Archiving a required policy that is missing now answers not found, and one
  that is already archived is a no-op, because both are decided before the
  precondition. The refusal of a live required policy is unchanged.
- Genesis for a read-only world is still skipped by the knowledge server, not
  by the store: it happens on the blob store before a `Store` exists.
- A provisioned world whose policy is archived or unenforceable now fails to
  open, where it used to open and refuse every write.
- Rejected for now: the broker handing the policy body to the server through
  the fragment, which needs a new config field and chart changes.
- Next release: the broker stops writing `bootstrap`, the server drops the
  field, and both fragment contract tests move together.
