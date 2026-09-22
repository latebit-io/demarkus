# ADR 0022: The gateway takes its dependencies, not the Server

Status: accepted (2026-09-21).

## Context

The broker package is one package of about 29,000 lines. Its 2026-08 sweep
named four seams (core, OAuth server, MCP gateway, storage), and the
architecture quality plan schedules the split as track 3d-3. Before the
directories can move, the code has to stop reaching across the seams.

The MCP gateway held a `*Server` and read through it: the config, the world
registry, the write token store, the clock, the provisioner, the auth and rate
limit middleware. The Server in turn held the gateway's state: the write token
store, the production QUIC pool, the provisioner and the gateway profile,
which `newMCPGateway` wrote back into the Server as a side effect so
`/me/install` could scope worlds the way the gateway did. The memory broker
enabled provisioning through a `Setup` hook that mutated the Server after
construction. `NewServer` took seven positional arguments, four of them nil in
most tests. Config carried six `*Ref` methods that resolve Secret locations
for stores that will live in three different packages, and several helpers
took `*Config` only to reach the registry inside it.

## Decision

- `gatewayDeps` names everything the gateway needs: the world registry, the
  issuer, the public URL, the MCP config, the realm, the composed verifier,
  the allowed domains, the shared subject limiter, a clock, a logger, the
  write token store and the provisioner (nil in static mode). Nothing in it
  is a Server method: the bearer gate (`gatewayAuth`) and the 401 challenge
  are gateway code, and the per subject limiter is a free function over a
  registry. `Run` builds one `ServerDeps` (`buildServerDeps`) and derives
  `gatewayDeps` from it (`gatewayDepsFor`), so both listeners verify with the
  same composed verifier and share one subject bucket per identity, which was
  a side effect of borrowing Server methods before and is explicit now.
- `Run` owns the gateway's lifecycle: it builds the write token store and the
  QUIC pool, closes the pool after the MCP listener drains, and wires the
  provisioner when the binary supplies a bucket backend (`RunOptions.Buckets`,
  taking `*ProvisioningConfig`) and the config enables provisioning. The
  `Setup` hook, `EnableProvisioning`, `MCPGateway`, `MCPGatewayWith` and
  `CloseMCPGateway` are gone.
- The Server keeps only the OAuth surface and the management API. It learns
  the gateway profile's tenant scoping at construction (`ServerDeps.TenantScoped`)
  from the same profile `Run` hands the gateway, so the two cannot diverge.
- `NewServer(cfg, ServerDeps)`: signer, composed verifier (`verifierWith`),
  store, optional discovery and id_token signer, logger, clock, the two rate
  limiters and the scoping are named fields. Nothing is composed or built
  inside the constructor that another listener also needs; the provisioner
  takes a `provisionerDeps` the same way.
- Secret locations are free functions in `secret_refs.go`, next to the pinned
  Secret names and keys, so each future package calls them without owning
  `Config`. `readableWorlds`, `authorizedWorlds`, `lookupWorld`,
  `tenantWorldFor`, `installableWorlds` and `newWorldPool` take the registry (and the issuer where
  identity keys are built), not `*Config`.
- `Claims` and the context and fingerprint helpers live in `claims.go`;
  `config.go` is four files: types, load, defaults, validate.
- The two authorize redirect helpers are an `authorizeReply` value carrying
  the validated redirect URI and client state.

## Consequences

- Broker behaviour, tool output, wire goldens and log text are unchanged; the
  memory broker's "provisioning enabled" line now comes from `Run` with the
  same fields. One ordering change at startup: the Secret store is built before
  OIDC discovery, so a broker with both misconfigured reports the store first.
- Test fixtures are one file, `helpers_test.go`, with a `gatewayFixture` that
  calls `gatewayDepsFor` on its own `ServerDeps`, so tests and `Run` cannot
  wire differently. The clock, the store and the provisioner are injected;
  no test patches a private field after construction.
- The directory split (3d-3b) can now be a near pure move: `gateway` imports
  `core` for the registry and config types and nothing from the OAuth server;
  `Run` is the one place that knows all four packages.
- Lint ratchet 123 to 121 entries: `NewServer`'s argument count and the
  authorize helpers' argument count are gone, `Run` is three statements
  shorter.
