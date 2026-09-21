# ADR 0021: A world registry that announces drops, DNS label world names, one tenant door

Status: accepted (2026-09-21).

## Context

The broker's `Config` was also its live world set: the provisioned tenants
were swapped into it at runtime, under a mutex inside the config struct. Four
caches were keyed by world name (the QUIC client pool, the write token cache,
the memory seeding state, the tenant graphs) and none heard when a world left.
A deprovisioned world stayed dialable from the pool, and because a tenant's
name is derived from its identity, the same person provisioned again got the
same name, found `seeded=true` from the old world, and was never given the
memory template in the new, empty bucket.

World names were any non empty string. They are the host of every
`mark://<name>/` tool URL, a graph key, and part of a Secret name, and hosts
compare lowercase everywhere else (ADR 0018), so an uppercase name loaded fine
and then missed lookups, split graph identity and produced an invalid Secret.

Tenant resolution had two doors. Tool calls went through a gate that
provisioned on first arrival and seeded; resource reads resolved only, so a
first time user attaching a resource was told "not authorized". A refused
identity cost a registry round trip under the provisioner's global lock on
every call.

## Decision

- `worldRegistry` holds the static worlds and the provisioned set, and
  announces every world that leaves through `OnDrop`, after the swap and
  outside its lock. A world also counts as dropped when its name comes back
  under a newer provisioning (the registry record's creation stamp). The pool,
  the token cache, the memory seeder and the tenant graphs subscribe. `Config`
  is loaded data plus one accessor to the registry.
- A world name is a DNS label: lowercase letters, digits, hyphens, at most 63,
  no hyphen at either end. Config load refuses anything else and never
  normalizes, because that would rename Secrets on a running deployment. The
  registry applies the same rule to provisioned names: a record written by
  hand or by an older build is kept out and logged, never served. With that, tool URLs
  treat the world name as case insensitive and `links.Target.Hostname` is
  lowercase like every other identity accessor.
- `admitTenant` is the one door into tenant mode: resolve, provision on first
  arrival, seed, carry the world on the context. Tool calls and resource reads
  both use it, then apply the same one world rule (`tenantOwns`), then seed,
  so a refused call writes nothing. An unknown tool is refused before it. A
  provisioning refusal that will not change soon (not admitted, at capacity,
  being removed) is repeated from memory for one minute per identity.
- `mark_lookup_all` takes its worlds from the call's scope, not the whole
  config, so a mistake in the gate cannot turn it into a cross tenant search.

## Consequences

- A broker config with a world name that is not a DNS label no longer loads.
  The error names the world and the rule.
- A first resource read provisions and seeds, like a first tool call.
- An identity that was just admitted by an operator may wait up to a minute
  for a remembered refusal to expire.
- A request in flight on a world at the moment it is dropped fails when its
  client closes. The world no longer exists, so that is the right answer.
- Not covered: a deprovision and reprovision inside the same second share a
  creation stamp and look like no change. `graphstore` has no per owner drop;
  only the knowledge profile seeds several owners and its worlds are static.
- `RunDeprovision` takes a context and options, stops on SIGINT or SIGTERM and
  after fifteen minutes; a run cut short converges when it is run again.
