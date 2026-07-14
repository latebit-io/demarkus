# ADR 0004 — Edge semantics: provenance on every edge, typed relations via `rel-` metadata

Status: accepted (2026-07-13)

## Context

Every edge in the document graph was a bare `{from, to}` pair at every layer:
`graph.Edge` in memory, `graphstore.StoredEdge` on disk, the two-column
`| From | To |` table in the published `/graph.md` export, and the MCP tool
outputs. The link extractor (`links.ExtractWithPositions`) already parsed the
link's label text and byte position and then threw them away; every edge
builder used the lossy destination-only `Extract`.

Two consequences. First, backlinks could not say *why* or *where* a document
links — no label, no source section, no occurrence count. Second, every link
was an untyped "mentions": knowledge graphs live on predicates (supersedes,
implements, depends-on), OKF types the nodes (ADR 0003), but nothing typed the
edges. ADR 0002 superseding its v1 draft was expressible only as prose.

This was gap 3 of the 2026-07-13 knowledge-layer analysis ("edges have no
semantics"). The constraint from that analysis: the agent owns judgment, the
server owns accumulation — so the enrichment must ride existing primitives
with zero server, protocol, or store changes.

## Decision

### Edge identity and provenance

`graph.Edge` and `graphstore.StoredEdge` gain `Rel`, `Label`, `Anchor`, and
`Count`.

- **Identity is `{From, To, Rel}`.** Plain body links have `Rel == ""`; a
  plain link and a typed relation between the same pair are two distinct
  edges.
- **Occurrences aggregate.** Duplicate `{From, To, Rel}` occurrences in one
  crawl add into `Count`; the first occurrence with a non-empty label supplies
  `Label` and `Anchor` (the enclosing section's GitHub-slug anchor, computed
  by `mdoutline.AnchoredLinks` from byte offsets).
- **Store merge is last-crawl-wins per source.** For every source the crawl
  actually fetched (any status except external/error), the stored outgoing
  edge set is replaced by the crawl's, so deleted links and dropped `rel-`
  predicates do not linger in backlink queries. Re-merging the same crawl is
  idempotent and counts never inflate. Sources the crawl did not read keep
  their stored edges.

### Typed relations: the `rel-` publisher-metadata convention

A metadata key `rel-<predicate>` declares typed relations from the document to
one or more targets:

```text
rel-supersedes: /adr/0002-metadata-alignment.md
rel-depends-on: /architecture.md, mark://other-world/spec.md
```

- **Hyphen, never colon.** Metadata keys allow only `[a-z0-9-]`; `rel:x` is
  rejected at five validation sites and OKF import already sanitizes it to
  `rel-x`. The hyphen form is the one that rides the existing channel intact:
  publish → `meta.rel-<predicate>` on disk → returned bare on FETCH.
- **Values are comma-separated refs**, resolved against the document's own
  URL (matching how body links resolve). Malformed refs (empty, internal
  whitespace), self-references, and an empty predicate (`rel-`) are skipped
  silently — bad metadata must never fail a crawl.
- Both crawlers ingest the convention as typed edges: the client graph crawl
  (`graph.RelEdges`, fed by the fetch callback's new `Metadata` field) and
  the federation crawler (which additionally applies its mark://-only,
  loopback-drop, host-normalization target filter). Ingestion is entirely
  client-side; the server stores the key opaquely like any publisher key.

### Surfaces

- The `/graph.md` export's Edges table widens to
  `| From | To | Rel | Label | Anchor | Count |` (label pipe-escaped).
  `ParseExport` accepts both this and the legacy two-column row, so published
  graphs in the wild keep parsing; legacy edges get `Count 1`.
- `~/.mark/graph.json` schema stays **v1**: the new fields are `omitempty`
  and old files load unchanged.
- One shared annotation renderer (`graph.EdgeAnnotation`) formats the terse
  display suffix — ` [supersedes]`, ` ("Getting started", #intro, x3)` — used
  identically by the local MCP, the broker gateway, and both backlink lists;
  pinned-literal tests on both sides guard drift.

## Consequences

- `mark_backlinks` and `mark_explore` answer "what links here, from which
  section, calling it what, how often, and with what relation" instead of a
  bare URL list.
- The graph can answer "what replaced this" (`rel-supersedes` backlink), not
  only "what mentions this".
- New predicates cost nothing: any `rel-<predicate>` key becomes a typed edge
  with no code change. Vocabulary curation is an agent/plugin concern, in
  line with the agent-as-librarian principle.
- Duplicate `AddEdge` calls now aggregate `Count` instead of being dropped;
  only formatters and the export observe the difference.
- When publish-time edge extraction lands in the store backends (roadmap),
  it must capture these same fields, and per the backend-parity principle the
  behavior lands in the storetest conformance suite first.
