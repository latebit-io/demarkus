# ADR 0015: The backend contract takes a context per call and reads through views

Status: accepted (2026-09-21).

## Context

`backend.Store` had no `context.Context` on any method, so `bucketstore` built
its own from `context.Background()` and neither a request deadline nor a
shutdown could cancel blob I/O. The contract also embedded `Reader` in `Store`,
which let the handler read outside a snapshot; carried a mutable `Catalog`
that both backends ignored; used `os.ErrNotExist` as its not found sentinel;
returned three values from the archive call; and kept `CurrentVersionResult`,
which no handler path called.

Two shapes were weighed for the context. Binding it to the view
(`OpenReadView(ctx)` only) adds the fewest arguments, but stores a context in
a handle, fixes one deadline for every read on the view and hides
cancellation from the method signatures. `database/sql` settles the same
question for transactions with a context on `BeginTx` and on every call.

## Decision

- `OpenReadView(ctx)` bounds acquiring the snapshot. Every `Reader` method and
  `CatalogReader.Lookup` takes a context first.
- `Store` is `OpenReadView`, `Publish(ctx, WriteRequest)`,
  `Append(ctx, WriteRequest)` and `SetArchived(ctx, path, archived)` returning
  `ArchiveResult`. It no longer embeds `Reader`: the handler reads through one
  view per request, so a response never mixes two committed states.
- `backend.ErrNotFound` is the not found sentinel. `backend.FromNotExist`
  marks a backend's own missing file error and keeps the cause in the chain.
- `Catalog.Put` and `Remove` are gone; a backend keeps its catalog current
  inside its own writes. `Handler` has one `Store` field; LOOKUP is always
  available.
- `handler.HandleStream(ctx, stream)`; the world runtime passes a context that
  carries the same deadline as the stream.
- The file backend reads local disk, so its context only stops a call before
  it starts. The bucket backend's contract view holds the snapshot and the
  request deadline, never a context; a read after `Close` fails as canceled.

## Consequences

- The conformance suite gained a canceled context case that both backends
  pass: every contract call refuses and a refused write leaves nothing behind.
- The raw `protocol/store.Store` no longer satisfies the contract; the
  `filestore` backend is the file conformance target, which is what
  production serves.
- The current version probe left the contract. Suites derive it from
  `Versions` through `backendtest.Direct`, a one call at a time adapter.
- Not done here: paging on `ListEntries` (S17), the mutation timeout starting
  after the commit token (S4), and removing the context held by bucketstore's
  internal per operation reader.
