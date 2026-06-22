# ADR 0003 — Default OKF `type` on publish

Status: accepted (2026-06-22)

## Context

ADR 0002 aligned the store's metadata vocabulary with the Open Knowledge Format
and added a `demarkus okf` import/export/validate codec. That made a single
demarkus document's content model OKF-compatible, but conformance was only ever
realized at `export` time: a normal PUBLISH does not require a `type`, and OKF's
one hard per-concept rule is a non-empty `type`. So arbitrary writes were not
OKF concepts, and "writes conform to OKF" was false.

We want OKF-typed documents to be a core, opinionated property of demarkus, not
an export-time afterthought — while not breaking the existing ecosystem (the
soul, the knowledge broker worlds, journals, ADRs, the docs site all publish
markdown with no `type`).

A hard gate that rejected typeless writes was rejected: it would break every
current writer (including the memory/knowledge MCP `mark_publish` path), and it
would make demarkus *more* opinionated than OKF itself, whose first design
principle is "minimally opinionated — only requires `type`."

## Decision

Make it opinionated **by construction, not by rejection**. On PUBLISH, when a
document declares no `type`, the server assigns the default `type` `Document`
(`protocol.OKFDefaultType`). Implemented in `handlePublish` via
`applyOKFTypeDefault`, after `extractPublisherMeta` and before the store write.

- **Reserved OKF files** (`index.md`, `log.md`) are exempt — OKF defines them as
  navigation/history, not concepts, and they carry no frontmatter.
- A document that **already declares a `type`** is stored unchanged.
- The default is real publisher metadata, so it participates in content/metadata
  duplicate detection: republishing a previously untyped document acquires the
  default once (one new version), then dedups normally.
- The same constant backs the export codec's `type` synthesis, so a world that
  predates this change still exports conformantly; for worlds written under it,
  export is pure passthrough.

The `export` codec keeps its own `type` synthesis as a belt-and-suspenders for
documents written before this change or sourced outside demarkus.

## Consequences

- **Every served demarkus document carries an OKF `type`** — the per-document
  OKF concept rule now holds for the whole store by construction. This is the
  maximal per-document conformance achievable at write time.
- **Non-breaking.** Existing writers keep working and simply gain a default
  type; the soul, journals, and ADRs stay valid and become export-ready.
- **Still not a served bundle.** This does not make a server serve or ingest OKF
  bundles — frontmatter is stripped on serve and the layout is `versions/`. Full
  bundle conformance remains an `export` concern (ADR 0002).
- One-time version bump per legacy document on its first republish (the added
  type changes its metadata).

## Deferred

- A `strict` mode (server config or per-world policy) that *rejects* typeless
  concept writes, for worlds that want hard enforcement (e.g. a data catalog).
  Kept as an opt-in, never the global default, so it cannot break a world that
  does not want OKF semantics.
- Defaulting/normalizing `timestamp` on write (currently `export` synthesizes it
  from the modification time; OKF lists it as recommended, not required).
