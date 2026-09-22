# ADR 0024: The plugin binary is packages by concern, and a harness is a value

Status: accepted (2026-09-22).

## Context

`tools/demarkus-plugin` is the one binary every demarkus plugin adapter calls.
Its lifecycle package (`provision`, 2,142 lines) held eight concerns and its
state package (`registry`, 1,065 lines) seven, with a nine argument join, a
mutable package global selecting the harness, three `--format` switch ladders
in `main`, three semver comparators and no test over the lifecycle functions
that had produced the last two regressions (findings T3 and T4). The
architecture quality plan (track 3e, findings T6 to T12, T18, T21, T22) asked
for the split, one host package and the seams the tests need, behaviour
preserved: plugin output, hook output, state files and log text byte identical,
with one exception the tests forced and the consequences describe, the report
of a server that dies at startup.

## Decision

- `internal/provision` is the orchestration and health reporting root
  (`Provision`, `Init`, `Status`, `HealthWarning`, `VerifyAuth`,
  `DetectServers`) over five packages: `release` pins and installs the
  binaries, `procscan` discovers processes and probes ports and binaries with
  every probe bounded and an unrunnable probe reported as unknown, `tokens`
  mints, migrates and verifies the plugin token, `server` spawns, adopts and
  restarts the managed server, `seed` publishes the first documents.
  `progress` is the one `[demarkus-memory]` stderr log. The dependency shape
  is `release` on `procscan`; `tokens` on `procscan` and the registry's
  `statefile`; `server` on `procscan`, `tokens` and `release`; `seed` on
  nothing but `progress`, taking its client and token as inputs; only the
  root on all of them. `config` owns the `~/.demarkus` layout (`BinPath`,
  `TokenPath`, the memory roots, the managed log and tokens paths) and both
  halves of `plugin-memory.conf` (`LoadConfig`, `SaveConfig`).
- `internal/registry` is a directory of packages: `statefile` is the
  transactional engine (lock, atomic write, prepare then apply with rollback,
  record field and slug validation); `catalog` reads and binds the memory
  catalog and owns the local memory's alias and the names a join may not
  take; `knowledge`, `promote` and `broker` each keep one registry or
  validation; `join` commits a memory join as one `joinPlan`; `mcpconfig`
  edits a harness's MCP config file. Shared test fixtures live in
  `registrytest` and `provisiontest`, which import only packages whose tests
  do not import them back.
- The catalog record has one codec, `config.MemoryRow`, and one records
  reader, `config.Records`; `MemoryRegister` is gone, a join is the only
  writer.
- `internal/host` holds what differs per harness: the hook payload shapes,
  the project directory variable, the MCP config file and its remote server
  entry, the once per session nudge, the pi proxy unwrap. `Claude`, `Cursor`,
  `Pi` and `OpenCode` are values; `ForHook`, `ForGate` and `ForMcp` resolve
  the `--format` and `--harness` vocabularies, which do not change.
  `mcpconfig` takes the host as a parameter.
- `internal/semver` is the one release version parser; each caller keeps its
  own leniency (`update` reads an unparseable version as 0.0.0, the drift
  probe says nothing).
- Test seams are package values: `release.BaseURL`, `tokens.DialProbe`,
  `statefile.Writer`, the managed ports in `provision`. The server stub the
  lifecycle tests run is a compiled program, because `ps` reports a script's
  interpreter, and it answers `--version`, binds its `-port` and ignores
  SIGHUP as the real server does.
- `scripts/check-identical-copies.sh` guards every hand copied adapter file:
  the eight bootstraps byte for byte, the branded hooks modulo the plugin
  label and surface flag, the TS `runBin` helpers per family.

## Consequences

- Plugin output, hook output, state files and log text are unchanged, except
  the startup death report below; the hook shapes are pinned by `host`'s tests. Two edge cases moved with the
  dedup and are recorded in the debt ledger: the memory-default listing
  normalizes a hand corrupted catalog row, and the update check reads a
  prerelease suffixed version as unparseable.
- The lifecycle has tests for `EnsureBinaries`, `server.Ensure`, `Init` in
  all three modes, `provisionLocked`, `VerifyAuth` and `HealthWarning`; the
  T3 probe regression is pinned in `procscan` and `server`, T4 in `main`.
- Writing those tests showed that a managed server which dies at startup was
  reported as spawned: the plugin process never reaped its detached child, so
  the exited process read as alive to `kill 0`. `server.Ensure` now waits on
  the child while the poll runs and reports the exit with the log tail.
- The pin bump workflow reads `release.go` (`ServerVersion`, `ClientVersion`).
- The lint ratchet shrinks by the join's argument and result limit entries
  and the paths move.
