# ADR 0013: Graph freshness follows source revisions

Status: proposed (2026-09-13).

## Context

Graph seeds discarded source etags and revisions. Local adjacency then won
indefinitely, even against a newer source observation. Body hashes cannot detect
metadata-only relation changes. Export times and immutable shard versions describe
the aggregate, not its source documents. Archive state changes without changing
the immutable document version or etag (SPEC 9.1).

## Decision

This replaces ADR 0004's last-crawl-wins merge clause. Its extraction, edge identity
and occurrence semantics remain in force; ADRs 0005 and 0007 govern identity.

### Source observation

Each node may carry an observation containing `source`, `view`, `revision`, `etag`,
`complete`, `observed_at`, `attempted_at`, and `problem`. Source is the canonical
logical document URL, never its dial endpoint, graph seed owner or snapshot path.
Broker graph URLs can be routing aliases; their underlying source remains intact.
Tenant stores and their validation state retain the existing isolation scope.

Revision is the FETCH document version. Etags describe raw immutable version bytes,
including publisher metadata. Neither body hashes, shard versions nor export times
establish source freshness. Legacy observations have no inferred revision.

`view` is `document` for ordinary extraction or `federation` for its existing
mark-only, loopback-filtered projection. `complete` means the entire outgoing set
within that view was observed, including empty sets
and confirmed absence. A traversal depth or admission cap does not invalidate a
fully extracted source. Failed or capped source attempts preserve the last-good
revision, etag, fields and existing occurrence counts, advancing only attempt state.
New partial edges remain qualified incomplete evidence.

Freshness is derived: `unknown` without a complete revision-bearing observation;
`stale` when that observation is older than five minutes, future-dated, or has an
unresolved problem; otherwise `fresh`. Fresh means recently observed, not proof
that no write happened since. Source status remains separate from the revision.

### Selection

Numeric revisions compare only for identical canonical sources. Higher source
revisions replace entire outgoing sets; lower revisions cannot add edges or replace
metadata. Equal revision and etag with equal adjacency is idempotent. Different
etags at equal revision, different sources, or differing equal-revision adjacency
are incomparable and disclosed as a revision conflict.

Equal source revision, etag and status across known extraction views prefers the
`document` view without comparing adjacency. Federation filtering legitimately
changes its set. Unknown extraction views cannot supersede complete observations.

Complete revision-bearing evidence is preferred to revisionless evidence of the
same or previously unidentified source. This does not compare their revisions;
it prevents revisionless local absence from blocking source validation forever.
Unresolved selection is deterministic: local complete evidence wins a tie; among
seed owners the lexicographically first owner wins. Complete sets are never
unioned across owners. A direct complete read resolves unknown legacy evidence.
Legacy-to-legacy updates from the same owner follow that owner's replacement, but
remain unknown. Unknown seeds cannot invalidate a revision-bearing local read.

Direct subsequent reads resolve same-revision archive and unarchive transitions.
Confirmed `not-found` or `archived` removes outgoing edges and retains any known
revision as a high-water mark. Seeds at that revision cannot resurrect adjacency;
a subsequent direct source read or strictly newer source revision can supersede it.
Seed times never order operational status. Missing revision information cannot
establish known freshness for a source with no previous revision.

Each seed owner retains its own candidates. Omission in an owner's next complete
generation withdraws that owner's claims only, not another owner's evidence and
not the source document itself. Local candidates persist independently in graph
store schema v4; v1 through v3 remain readable as unknown source observations.

The highest observed revision for each source persists independently of owners.
Withdrawing a newer owner can expose older surviving topology, but cannot make it
fresh. Such rows carry `highest_revision` and a revision-regression problem through
exports and snapshots until a source observation reaches that high-water mark.
Pre-owner disk formats retain unowned seed evidence as unknown until revalidation.

### Validation and consumers

Backlink and exploration reads validate due backlink sources with existing FETCH:
at most eight attempts, no edge traversal, 8 MiB aggregate network/decoded budgets,
128 KiB per-source output budget and a fifteen-second caller-derived deadline.
Validation order is oldest source attempt then URL. Per-source single-flight and
a five-minute retry cooldown bound repeated failures. Remaining due sources and
attempt/request/byte counters are visible. Explicit graph crawls retain their
existing bounded neighborhood contract. TUI graph opening also validates incoming
sources within this bound. A seed's not-modified response validates no sources.

Graph rows, backlinks, exploration, TUI and CLI share freshness annotations.
Errors remain visible alongside retained evidence. Validation does not select
evidence for answers or enable automatic graph-assisted retrieval.

### Generated formats

Strict manifests and shards advance to `demarkus-graph-snapshot/v2` and
`demarkus-graph-snapshot-shard/v2`. Node JSON gains `observation`; edges and
immutable descriptors retain their meaning. New readers accept v1 as unknown and
validate v2 observations atomically. Manifest and shard format versions must match.
Old v1 readers reject v2 and retain their previous store, rather than misread it.

The separately published `/graph.md` retains its existing node and edge tables.
An additive `## Source observations` fenced JSON array maps each node URL to its
observation. Existing table readers ignore this section. New strict readers reject
malformed, duplicate or unmatched observation rows before applying any topology.
Absent sidecars remain valid unknown-freshness exports. Federation still publishes
only complete generations, with immutable shards verified before manifest CAS.

## Consequences

- Newer source evidence can replace local topology without permitting old seeds
  to regress it; metadata-only relation updates are visible.
- Snapshot v1 clients need upgrading to ingest v2 snapshots. Legacy markdown
  consumers continue reading topology, without freshness semantics.
- Validation costs up to eight source reads per consuming call; five-minute
  cooldown suppresses repeats. Failure and incomplete scope remain explicit.
- Source version numbers assume the protocol's append-only source history. A
  source reset or conflicting version bytes stays incomparable; no artificial
  clock is derived from exporter timestamps to hide it.
- Real-soul lookup/fetch token totals remain ordinary-retrieval regression data.
  Freshness correctness and revalidation cost are measured directly; graph-enabled
  answer evaluation remains a separate experiment.
