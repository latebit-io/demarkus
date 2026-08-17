# ADR 0005 — Node identity omits the default port

Status: proposed (2026-08-17)

## Context

A graph node is identified by its URL string and compared exactly: the store
looks up `s.nodes[url]` and matches backlinks with `e.To != url`, with no
normalization anywhere. Identity is therefore whatever spelling the writer
used, and two spellings of one document are two documents.

Today the intended canonical form is `mark://host:port/path` with the default
port filled in, because `fetch.ParseMarkURL` supplies `protocol.DefaultPort`
when the address omits one. That intent was never enforced. `demarkus-mcp`
rebuilt the canonical form explicitly before crawling; the CLI, TUI, and broker
passed through whatever the caller typed. PR #318 closed the crawl entry by
canonicalizing inside `graphstore.CrawlAndPersist`, using the caller's own
`parseURL` so each transport keeps its own rule.

The damage was real before that fix. A rebuild of the demarkus-soul graph
produced 388 nodes for 192 documents, 204 keyed `mark://soul.demarkus.io/...`
and 167 keyed `mark://soul.demarkus.io:6309/...`. A second symptom rode along:
`EtagFetcher` keys etags canonically while nodes were keyed raw, so `Merge`
never attached an etag from a portless crawl and every conditional re-crawl
silently degraded to a full refetch.

Three doors remain open after PR #318:

- `SeedFromExport` ingests rows from another producer's `/graph.md` verbatim.
- The read paths (`GetNode`, `Backlinks`, `BacklinksEnriched`) trust the
  caller's key. The broker's `handleMarkBacklinks` validates the URL shape and
  then queries with the agent's raw string, so a near-miss returns "no
  backlinks" rather than an error.
- `links.Resolve` returns any destination containing `://` untouched, so an
  absolute body link written without a port creates a second node even under a
  canonical crawl.

Two further facts shape the decision. The system already separates identity
from dial address: `mark_worlds` returns a world's name and its dial address as
distinct columns, and the broker addresses worlds as `mark://{worldName}/{path}`
with no port at all, rejecting a port outright as an operator typo. And humans
and agents naturally write `mark://host/doc.md`, which today is the wrong
spelling.

## Decision

**Node identity is the authority with the default port omitted.**

- `mark://host/path` when the server listens on `protocol.DefaultPort`.
- `mark://host:7000/path` when it does not. A non-default port stays part of
  identity because it names a different server.
- The broker's `mark://{worldName}/{path}` form is unchanged. It is already
  portless, so both transports now follow one rule rather than two.

### Identity is not the dial address

`fetch.ParseMarkURL` keeps filling in the default port. Dialing needs a port;
identity does not. These are separate functions with separate jobs, and
conflating them is what produced the original bug. Canonicalization for
identity gets its own exported helper, and the transport keeps its own.

### Normalize at every keyed door, not just the crawl

The store takes an injected canonicalizer, following the existing `parseURL`
pattern so the store never hardcodes a rule that is wrong for the broker.
Hardcoding "strip 6309" inside `graphstore` would rewrite `mark://servicing/x`
as `mark://servicing:6309/x` and corrupt every brokered world key.

It is applied on ingest (`Merge`, `SeedFromExport`, the crawl entry) and on
read (`GetNode`, `Backlinks`, `BacklinksEnriched`). Read-side normalization is
not redundant with ingest: a mismatch on read returns an empty result, which is
indistinguishable from a document that genuinely has no backlinks, and a silent
wrong answer is the failure mode this whole class of bug keeps producing.

### Accept both forms forever

Ingest accepts either spelling and stores the stripped one. Published graphs
written under the old rule keep parsing, and their rows are stripped on the way
in. `graph.json` bumps `schemaVersion` from 1 to 2 and strips keys on load, so
existing stores migrate instead of being rejected by the version check.

There is no flag day and no coordinated release. Hub aggregates can be
republished whenever it suits.

## Consequences

- The spelling people naturally type becomes the correct one, which closes the
  absolute-body-link hole by making the common case canonical rather than
  wrong.
- Identity matches RFC 3986 section 6.2.3, which removes a scheme's default
  port during normalization, and matches the broker's existing split between a
  world's name and its dial address.
- `/graph.md` row shape changes. Producers are the `demarkus-agent` federation
  crawler, which publishes hub aggregates, and `mark_graph_publish`. Consumers
  are the MCP seed path, the broker seed path, and the library floor, which has
  broken once before on an export format change. Accept-both keeps every one of
  them working across the transition.
- Moving a world off the default port changes the identity of every node it
  serves. This is already true and is not a regression, but it means a port
  remains load-bearing whenever it is not the default.
- Tests that pin the old rule change with it: the fedcrawl producer-consumer
  contract test, the MCP and broker seed tests, and
  `TestCrawlAndPersistCanonicalizesStartURL`, whose expectation inverts.
  `TestCrawlAndPersistKeysMatchAcrossURLForms` survives untouched, because it
  asserts that the two spellings agree rather than which one wins. That is the
  test carrying the real invariant.

## Alternatives rejected

- **Keep the port always.** Internally consistent, and it is what the current
  data already looks like. Rejected because it leaves the natural spelling
  permanently wrong, keeps two identity rules across the two transports, and
  does nothing about absolute body links.
- **Normalize on read only.** Cheaper, and it would answer queries correctly
  today. Rejected because stored data stays forked on disk, so every export and
  every seed keeps propagating both spellings.
- **Hardcode the rule inside `graphstore`.** Rejected as wrong for the broker,
  as above.
- **Reject non-canonical input.** Honest, and it surfaces the problem instead
  of hiding it. Rejected because a store seeded from another world's `/graph.md`
  cannot control what that producer wrote, and refusing to seed is worse than
  normalizing.

## Deferred

- Whether the LOOKUP catalog and the OKF export should adopt the same identity
  rule. They key by path within a world today and are unaffected.
- Case normalization of the authority. `mark://Host/x` and `mark://host/x`
  remain distinct until there is evidence anyone writes the former.
