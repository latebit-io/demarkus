# ADR 0002: Align store frontmatter with the Open Knowledge Format

Status: accepted (2026-06-22)

## Context

Google published the Open Knowledge Format (OKF) v0.1 on 2026-06-12, a
vendor-neutral format for organizational knowledge. Its model is close to
demarkus's own: directory bundles of markdown, a cross-link relationship graph,
`index.md` hubs, and path-minus-`.md` as concept identity. Spec:
https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md

The one structural difference is where metadata lives. OKF carries it **in the
document body** as YAML frontmatter (required `type`; recommended `title`,
`description`, `resource`, `tags` as a YAML list, and `timestamp`). demarkus
carries metadata **out of band**: the on-disk version file is prefixed with a
store-managed frontmatter block that is stripped before the body is served, and
the catalog reads only the publisher keys. Syntactically both use a `---`
block, but the data model differs.

Before this change, every publisher key was namespaced under a `meta.` prefix
on disk (`meta.tags`, `meta.type`). That prefix was not cosmetic; it was the
integrity boundary that prevented a publisher from supplying `archived: true`
or `version: 999` and forging store state, because `extractMetadata` read only
`meta.*` keys.

We are pre-1.0, so the on-disk storage format is still free to change.

## Decision

Adopt a **split namespace** in the store frontmatter so the persisted field
names match OKF for the fields it defines, without losing the integrity
boundary.

- **Recognized OKF fields** (`type`, `title`, `description`, `resource`,
  `tags`, `timestamp`) are written **bare**. `tags` is serialized as a YAML
  flow list (`tags: [a, b]`) to match the spec exactly.
- **Non-spec publisher keys** (e.g. `importance`, custom fields) keep the
  `meta.` prefix, demarkus's own metadata lane, and the anti-forgery boundary
  for the open-ended key set.
- **Reserved store fields** (`version`, `previous-hash`, `archived`) stay bare
  and unchanged. The boundary the prefix used to enforce is now enforced by
  name: a `reservedMetaKeys` denylist that `validateMeta` rejects and
  `extractMetadata` never surfaces as publisher metadata.

Implementation is contained to `protocol/store/store.go`:
`buildVersionFile` (write), `extractMetadata` (read), `validateMeta` (reject
reserved). The **in-memory metadata map stays bare-keyed**, so the catalog,
handler, and filter layers are untouched; `tags` round-trips list↔csv, so the
map contract (`"a,b"`) is unchanged. The spec change is recorded in SPEC.md
§9.4 / §8.1.

## Consequences

- **Back-compatible reads.** `extractMetadata` still parses pre-change
  `meta.tags` / `meta.type` writes, so existing immutable versions and live
  servers (e.g. `soul.demarkus.io`) keep working. New writes use the bare form.
- **Vocabulary alignment, not interop.** A single demarkus document's content
  model is now OKF-compatible, but the store frontmatter is stripped before
  serving and the `versions/` layout is not an OKF bundle tree. demarkus does
  **not** yet serve or ingest OKF bundles. We deliberately do not claim "OKF
  compatible" as a normative conformance guarantee.
- **Sets up a 1:1 codec.** Because the persisted field names are now the literal
  OKF names, a future import/export codec is a direct mapping rather than a
  translation table.
- **Relationship framing.** At the system level demarkus remains a superset: it
  layers versioning, a hash chain, QUIC transport, capability auth, and LOOKUP
  discovery on top of an OKF-compatible document.

## Deferred

- An import/export codec (OKF bundle ↔ demarkus world) with frontmatter
  reattachment on the served document and `index.md` / `log.md` conventions.
- A conformance check against the OKF reference sample bundles.
- Scope decision parked: import-only vs round-trip vs server-native
  content-negotiated bundle serving; and where it lives (`tools/demarkus-okf`
  CLI vs `client/okf` package).
