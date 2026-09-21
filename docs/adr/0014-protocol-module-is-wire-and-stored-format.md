# ADR 0014: The protocol module is wire and stored format only

Status: accepted (2026-09-21).

## Context

`protocol/store` held two things: the pure model every backend shares (the
version file format, metadata rules, path canonicalization, document types,
error sentinels) and the filesystem backend. The client needed four pure
helpers from it and so linked a filesystem store; `bucketstore` imported a
file backend to name a `Document`. `protocol/token` carried flock and TOML
file I/O that only the tools module used. The frontmatter split was written
seven times with two grammars, and `ParseResponse` refused the close at end
of input form that `ParseRequest` accepts.

The filesystem store cannot move to `server/`: tools imports it
(`demarkus-publish`, migration) and must not import server.

## Decision

- `protocol/storefmt` owns the stored document model and does no I/O. Every
  backend, the handler, the client and the plugin gate import it.
- `protocol/store` is the filesystem backend only. It stays in the protocol
  module so tools can use it. Only the server's file backend wiring, its test
  suites and `demarkus-publish` import it.
- `protocol.SplitFrontmatter` is the one splitter for requests, responses and
  stored versions. Both wire parsers accept the close at end of input form;
  the stored grammar still requires a newline after the closing fence.
- `token` lives at `tools/internal/token`. The protocol module no longer
  depends on a TOML library.
- The deprecated `LookupHash`, `CurrentVersion` and `Archive` shims and the
  one line export wrappers are gone; `protocol.ReservedMetadataKeys` is now
  the predicate `protocol.IsReservedMetadataKey`.

## Consequences

- `go list -deps ./client/...` shows no filesystem store.
- The `Migrator` interface ADR 0010 kept is now `storefmt.Migrator`.
- Out of tree importers of the removed names break at compile time; there is
  no compatibility layer.
- Tests of pure format functions still sit in `protocol/store` and move when
  that test file is split.
