# ADR 0023: The broker is four packages behind one Run

Status: accepted (2026-09-22).

## Context

ADR 0022 took the reaching across the broker's seams out while everything
still sat in one package of about 29,000 lines. The architecture quality plan
(track 3d-3, decisions F10 and F11) schedules the directory split as the
second of two PRs: near pure moves, with the names another package calls
exported and nothing else.

Two things could not be pure moves. The gateway's dependencies were built in
`Run` from the OAuth server's `ServerDeps`, and the test fixture mirrored that
call; after the split neither a gateway test nor a shared fixture package can
reach `Run` without an import cycle. And the sweeper took the OAuth server's
`*RefreshStore`, which the storage package cannot import without a cycle.

## Decision

- `tools/internal/broker` is four packages and a test fixture package. `core` holds what every package
  shares: the config and its validation, the world registry, claims and
  authorization, the Secret locations and the `SecretStore` contract, the
  OIDC verifier and id_token signer, the rate limit registry and the subject
  middleware, the bearer extractor. `oauthsrv` is the management API and the
  OAuth surface, with the state cookie signer that only it uses. `gateway` is
  the MCP gateway with the pool, the write token store, the profiles and the
  memory seed. `storage` is the two Secret store backends, the tenant
  provisioner, the GCS bucket backend and the sweeper. `brokertest` carries
  the fixtures two or more packages share and imports `core` only.
- The dependency shape is `gateway` on `core` and `storage`, `oauthsrv` on
  `core`, `storage` on `core`, and only the root package on `gateway` and
  `oauthsrv`. The root keeps `Run`, `RunDeprovision`, their option types and
  the wiring helpers, and re-exports the four names the binaries pass in
  (`Config`, the two profiles, `NewGCSBuckets`) so the binaries import only
  the root.
- `core.SharedDeps` names what both listeners share: the composed verifier,
  the subject limiter, the clock and the log. `oauthsrv.ServerDeps` and `gateway.Deps` embed it;
  `gateway.DepsFor` takes it by value. `Run` and every fixture build the
  gateway's dependencies through `DepsFor`, so the two listeners cannot be
  wired differently.
- `storage.Sweeper` sweeps through a one method `RefreshSweeper` interface that
  the OAuth server's `RefreshStore` satisfies.
- A package's own tests use its unexported helpers; fixtures shared across
  packages live in `brokertest`; `core`'s tests repeat the few fixtures they
  need because `core` cannot import `brokertest`. Cross listener tests are
  split per listener: each gate is pinned against the same identities in its
  own package.
- Exporting is limited to what another package calls. Names that stutter with
  the package (`GatewayProfile`) shorten (`gateway.Profile`); the config's
  `worlds()` accessor is `Registry()` because `Worlds` is the loaded field.

## Consequences

- Broker behaviour, tool output, wire goldens and log text are unchanged; the
  kind smoke with tool calls passes as before. The lint ratchet holds at 121
  entries with paths moved.
- `labels.go` (`NewLabel`, `LabelPrefix`) had no caller since PR 380 and is
  gone; the package doc it carried is `doc.go`.
- The mechanical part of the move (qualifying about 1,900 cross package
  references and adding imports) was done from the single package's type
  information before the files moved, so no reference was rewritten by hand.
- What each package exports is now the seam's contract; widening it needs a
  reason another package calls the name.
