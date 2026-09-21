# ADR 0019: Tool bodies and the write contract live in shared packages

Status: accepted (2026-09-21).

## Context

The `mark_*` MCP tools were implemented twice, in `client/cmd/demarkus-mcp`
and in the broker gateway, about 900 lines on each side, and the copies had
drifted: explore, index, discover and backlinks already answered differently.
The handler bodies lived in `package main`, so neither side could call the
other's, and exported client APIs took `client/internal` types, so a tool
could not reuse them either.

Writes had three contracts. The MCP tools required a version, merged on
conflict and reconciled a lost response in one mode only. The CLI published
without any version check by default, required an explicit version for APPEND,
handled an `edit` conflict by dropping a temp file, and stripped a document's
metadata on every edit, because PUBLISH replaces the metadata map and `edit`
sent none. No surface showed the server's reason for a refusal in the default
publish mode: the merge path dropped the response body, which is where a
publish policy lists its violations.

## Decision

- `client/marktools` owns the body of every `mark_*` tool. It takes a
  `Backend`, the seven method, context first client that the direct client and
  the broker's world dispatcher both already are, and hooks for what differs
  between surfaces: `Resolve`, `ReadToken`, `Writer`, `Agent`, `Seen`, `Graph`,
  `ErrText`, `Now`, `Warnf`. A tool returns `Result{Text, IsError}`; the package
  does not depend on mcp-go, and formatting stays in `mcpfmt`. A surface is
  argument parsing, authorization and hook bindings.
- Where the order of refusals is part of a tool's output, the body owns the
  order. The surface keeps only the checks that come first.
- `client/docwrite` is the one write contract, below `marktools` and beside
  the CLI: PUBLISH with a merge candidate on conflict, APPEND that resolves its
  version, ARCHIVE, and for all three a look at the head, never a resend, when
  a response is lost. It returns outcomes; `marktools` words them as tool text,
  the CLI as its own output and exit code.
- The CLI adopts that contract. `PUBLISH` requires `-expected-version`;
  `-force` is the old unchecked overwrite. A stale version prints the merged
  candidate on stdout and exits 1. `APPEND` resolves its version when none is
  given. `edit` is version checked, carries the document's metadata, and saves
  the merged candidate or the edits to a file on any failure.
- A conflict with no base to merge against (the claimed version never existed,
  or retention pruned it) is reported as the conflict, not as an error.
- A publish result carries the server's response body, so a refusal reads with
  its reason on every surface and in every mode.
- `client/generation` owns the slot publication algorithm once; the hash index
  and the graph snapshot are specs over it. `client/listwalk` is the one LIST
  walker, with a policy hook for what a problem costs.

## Consequences

- The client MCP server's `main.go` went from about 1350 lines to 555. The
  broker keeps its copies until it adopts `marktools`; until then a bug fixed
  in one copy is fixed in the other, as was done for the dropped refusal body.
- Tool output is unchanged except for defects: a refusal now shows its reason,
  a conflict without a base reads as a conflict, an unreconciled lost response
  says the write may have landed, the client no longer prints a doubled
  `invalid URL:` prefix or a dial port in the graph publish line, and explore
  says when more siblings exist.
- Scripts that ran `demarkus -X PUBLISH` without a version must add
  `-expected-version` or `-force`. The repository's seed scripts use `-force`.
- Six places where the two surfaces differ only in wording stay different
  until the broker moves; making them identical changes broker output and is
  decided then.
- Behavior shared by surfaces is tested at the owning layer; a surface keeps
  argument, hook and one happy path test per tool.

## Amendment (2026-09-21)

The broker adopted `marktools` and `docwrite` the same day, so the two
consequences about its copies and about six wording differences no longer
hold. What replaced them, including the order of checks and who owns a lost
response, is [ADR 0020](0020-one-reconcile-owner-and-check-order.md). The
"does not depend on mcp-go" claim holds for direct imports only: `mcpfmt`
still reaches it, which the package split is to fix.
