# ADR 0008: GCS world state commits through one root CAS

Status: accepted (2026-08-22). Store conformance, independent-store CAS races,
and real-GCS sizing at 100,000 documents in each of three worlds passed.

## Context

The multi-world knowledge server needs durable storage shared by two or more
replicas. GCS provides strongly consistent object reads and generation
preconditions, but it does not provide transactions across object names.

Per-document conditional writes are insufficient. Concurrent creation of
`/a.md` and `/a.md/b.md` can otherwise violate the document-versus-directory
invariant. Periodically rebuilt per-pod hash and LOOKUP indexes can also return
stale negative answers after another replica commits a write. Both outcomes
violate backend parity.

The initial workload is read-heavy: up to 100,000 documents and less than one
committed mutation per second per world. That permits one world-level
linearization point if reads can reuse immutable state efficiently.

## Decision

### One bucket and one commit point per world

Each world uses its own GCS bucket and has an immutable random world ID. The
bucket name is deployment configuration, not world identity. Moving or restoring
a world preserves its world ID.

`_demarkus/v1/head.json` is the only mutable object and the sole linearization
point for every world mutation. It records the schema version, world ID,
monotonic sequence, immutable root key and hash, and a bounded set of recent
operation receipts. Every replacement uses the previously observed GCS object
generation as a precondition.

All referenced state is immutable:

```text
_demarkus/v1/head.json
_demarkus/v1/roots/<root-hash>.json
_demarkus/v1/index/<00..ff>/<shard-hash>.json
_demarkus/v1/docs/<path-hash>/manifests/<manifest-hash>.json
_demarkus/v1/history/<history-hash>.json
_demarkus/v1/blobs/<stored-bytes-hash>
_demarkus/v1/pins/<backup-id>.json
```

A root references 256 namespace and catalog shards selected by the first byte
of the canonical path hash. Each entry contains the canonical path, manifest
reference, current version, archive state, current body hash, modified time, and
complete LOOKUP catalog entry. Archived documents continue to reserve namespace
topology but are absent from live body-hash and catalog indexes.

Document manifests reference retained history chunks. History entries contain
the version number, exact stored-version blob reference, body hash, and modified
time. Raw document paths are never authoritative object names.

### Immutable data has deterministic identities

Immutable JSON uses UTF-8, no insignificant whitespace, fixed schema field
order, lexicographically sorted map keys, schema-defined array order, and no
floating-point values. Its identity is lowercase hexadecimal SHA-256 of those
exact bytes. Readers recompute the hash and verify both the object key and every
parent reference before accepting an object.

Blob objects contain the exact bytes produced by `store.SerializeVersion` and
are create-only. Their key is the SHA-256 of those bytes. A protocol ETag is the
same raw stored-byte SHA-256 rendered without the `sha256-` prefix. GCS
generation, MD5, CRC32C, root hashes, manifest hashes, and body hashes are never
used as protocol ETags.

Archive and unarchive commits reuse the document's exact blob and history
objects. They create only a manifest, changed shard, and root that record the
new operational archive state; version bytes, history entries, ETags, and
modified times remain unchanged.

### Every request uses one validated snapshot

Each protocol request performs a strong read or conditional validation of
`head.json`. There is no time-based freshness window. An unchanged generation
reuses cached immutable state. A changed generation loads the new root and only
the shards whose hashes changed, rebuilds derived indexes, and atomically
installs a new snapshot.

One request pins one root snapshot for path lookup, FETCH, LIST, VERSIONS, chain
verification, hash resolution, LOOKUP, and publish-policy evaluation. A failure
to validate the current head returns `server-error`; replicas never serve a
known-stale snapshot during an outage.

### Writes stage immutable state, then replace the head

Every mutation follows this sequence:

1. Read and pin the current head and root snapshot.
2. Canonicalize and authorize the path against the selected world.
3. Evaluate policy and all protocol and namespace preconditions against that
   snapshot.
4. Create the blob, history, manifest, changed shard, and root as immutable
   objects.
5. Replace `head.json` with an if-generation-match precondition.

A successful head replacement commits the entire candidate. A failed
replacement exposes none of it. The loser reloads the head and revalidates all
conditions before rebasing; a target-document change returns the normal Mark
conflict. Staged objects that lost a race remain unreachable until garbage
collection.

Each mutation has an operation UUID. Candidate heads retain receipts for the
last 16 committed mutations. This exceeds the full 10 second reconciliation
window at the supported envelope of less than one committed mutation per second.
After a timeout or lost response, a writer rereads the head: a receipt proves
success, a newer head whose receipt window still covers the candidate sequence
proves failure, and an unchanged head permits retry of the identical candidate.
An evicted candidate sequence has an indeterminate outcome: the server returns
`server-error`, logs the operation UUID, and MUST NOT replay the mutation. The
same applies when reconciliation cannot read the head before the deadline.

### Migration, backup, and reclamation follow the root graph

Bulk import creates all immutable state and publishes one initial root and head,
rather than committing once per document. Verification compares exact stored
bytes, versions, chains, archive state, catalog state, and namespace topology.

Backups pin an immutable root under `_demarkus/v1/pins/` before copying or
retaining reachable objects. Garbage collection is an asynchronous two-pass
job. It marks from the current root, retained prior roots, active import roots,
and backup pins, then waits a grace period longer than the maximum mutation,
import, and clock-skew window before marking again. Only objects unreachable in
both passes and older than the grace cutoff are eligible for deletion, so a
concurrent writer's staged objects cannot be swept.

Before the second mark, the collector CAS-replaces the head with the same root
and an active sweep epoch. This generation change invalidates every staged
writer candidate. All writer head replacements require an inactive sweep epoch,
so writers rebase only after the collector conditionally clears it. The head
barrier therefore prevents a writer from committing an old unreachable object
between the collector's final head check and deletion.

A backup creator must observe an inactive sweep epoch, create its pin, then
re-read the head before relying on that pin; if the epoch changed or became
active, it retries after the sweep. The collector scans pins only after
activating the barrier, revalidates the head generation before each delete
batch, and aborts on any change. This handshake fences pin creation while the
age rule protects objects staged before barrier activation. Logical retention
takes effect in the committed manifest immediately; physical deletion occurs
later and never changes protocol-visible history.

The active epoch references an immutable deletion plan containing each object
key and observed GCS generation. Every delete uses that generation as a
precondition. A crashed collector is recovered by continuing the same plan
while the head barrier remains active, then verifying every planned generation
is absent before conditionally clearing the epoch. Recovery never clears or
steals an epoch based only on elapsed time. A stale worker can therefore delete
only a planned old generation; after the barrier clears, recreation of the same
content-addressed key receives a new generation and is fenced from stale delete
requests.

## Isolation boundary

One bucket per world provides independent lifecycle, restore, accounting, and
accidental-deletion boundaries. Per-world tokens, policies, caches, limits, and
logs remain separate in memory. The process, workload identity, resource limits,
readiness, and crash domain are shared; this design is logical isolation within
one enterprise trust boundary, not hostile multi-tenancy.

## Consequences

- Every visible mutation is globally ordered per world, including unrelated
  paths and policy changes.
- Two replicas sharing only GCS observe the same head on the next request without
  sleeps, polling intervals, or replica-local negative-cache staleness.
- Concurrent document and descendant creation cannot both commit.
- Read cost includes one strong head validation per request; changed roots load
  only changed shards.
- One object name limits sustained commits to roughly one per second per world.
  The GCS implementation must reconcile ambiguous outcomes and apply bounded
  retry with jitter; deployments must remain inside the measured write envelope.
- Failed writes may leave unreachable immutable objects. Until the fenced
  asynchronous collector is implemented, they intentionally accumulate;
  retention remains logical and no request path performs physical deletion.
- The default filesystem server remains free of the GCS SDK. The knowledge
  server is a separate binary and image, consistent with ADR 0006.

## Sizing validation

Production implementation proceeded only after a real-GCS spike measured three
100,000-document worlds: cold load, warm head validation, LOOKUP latency, memory,
write throttling, conditional-write retries, and cleanup cost.

GCS is the required backend behind the same server storage interfaces as the
filesystem store. Measurements establish resource requests, cache shape,
concurrency, retry policy, and the supported write envelope. An unacceptable
measurement requires optimizing the GCS design or narrowing its documented
capacity, not adding a Postgres coordinator. Path topology, immediate hash and
LOOKUP freshness, policy atomicity, and one-request snapshot consistency remain
non-negotiable.

### Measured gate

The full gate ran on 2026-08-22 against three disposable US multi-region buckets
with 100,000 documents per world, 64 seed workers, 100 warm validations, 100
LOOKUP runs, 20 same-generation CAS contenders, and 20 serial head writes at a
1.5 second interval. Every bucket held 300,258 objects. All resources were
deleted after measurement; local credentials expired during automatic cleanup,
so the final two buckets were removed manually after reauthentication.

| Metric | World 1 | World 2 | World 3 |
|---|---:|---:|---:|
| Seed 100,000 documents | 15m29s | 15m37s | 16m20s |
| Cold snapshot load | 3.81s | 4.15s | 3.67s |
| Snapshot heap increase | 132.6 MiB | 133.5 MiB | 135.4 MiB |
| Warm head validation p99 | 70.2ms | 66.4ms | 71.9ms |
| LOOKUP p99 over 100,000 entries | 42.6ms | 48.8ms | 52.2ms |
| Contended CAS p99 | 502.9ms | 523.2ms | 610.8ms |
| Serial head write p99 | 6.54s | 3.10s | 1.87s |

The three loaded snapshots used 396.7 MiB aggregate Go heap and reached 916.3
MiB peak process RSS. Each contention run produced exactly one winner and 19
expected precondition failures. All 60 serial writes committed. GCS returned 23
HTTP 429 responses across contention and serial writes; bounded application
retry reconciled every one. No 5xx or timeout occurred. Deleting one complete
300,258-object world took 20m23s. The 1.44 million HTTP attempts also included
63 transient transport errors that did not prevent a measured operation from
completing, reinforcing the requirement to treat ambiguous outcomes explicitly.

Initial supported limits are therefore 100,000 documents per world and less
than one committed mutation per second per world. Keep the 1.5 second local
commit pacing, bounded jittered retry, and 10 second request deadline. Provision
a 2 GiB memory limit for a process serving three fully loaded worlds. Garbage
collection and bulk world deletion must be asynchronous operational jobs; they
must not run in a request path.

## Alternatives rejected

- **Per-document CAS.** It cannot atomically enforce cross-path topology or
  update all derived indexes.
- **Periodic catalog refresh.** It returns stale LOOKUP and hash misses across
  replicas.
- **One shared bucket with world prefixes.** It weakens lifecycle and restore
  boundaries without reducing the accepted process-level IAM blast radius.
- **gcsfuse.** Filesystem emulation does not supply the required transaction and
  snapshot semantics.
- **Postgres as a coordinator.** It would split one storage backend across two
  systems and make GCS subordinate to a database. The GCS store implements the
  existing server storage contract directly.
- **Serve cached state when head validation fails.** This violates the selected
  immediate-consistency contract and can return false not-found results.
