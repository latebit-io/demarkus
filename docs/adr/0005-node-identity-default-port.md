# ADR 0005: Node identity omits the default port

Status: accepted (2026-08-18). Shipped in PR #320 (`2964a35`); the managed
binary pins that close the mixed-version window followed in #319 and #321.
Amended before acceptance from what implementing it taught: three claims below
were wrong as first written and are corrected in place, and the "What
implementation changed" section records what moved and why.

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

One package-level function, `links.CanonicalURL`, is enough. Stripping a
default port is correct for both transports: a brokered world name carries no
port, so the rule is a no-op there. Injection was specified in the first draft
out of caution and proved unnecessary. The caution was real but belonged to the
*old* rule: hardcoding "add 6309" would have rewritten `mark://servicing/x` as
`mark://servicing:6309/x` and corrupted every brokered world key. Direction
matters, and only one direction is safe to hardcode.

Identity is built in one place too. `links.NodeURL(host, path)` constructs it
from parsed parts, so no caller assembles an identity by concatenation. A test
pins `NodeURL(h, p) == CanonicalURL("mark://" + h + p)` so the constructor and
the normalizer cannot drift.

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

Accept-both is what removes the flag day, and it has to, because the published
`/graph.md` changes shape the moment this ships. The agent's export is rendered
from a `graphstore`, so canonicalizing the store necessarily canonicalizes the
export. The wire format is not a separate decision that can be sequenced later.
What accept-both buys is that no consumer has to upgrade in lockstep: an old
export lands correctly in a new store, and a new export lands correctly in any
consumer that canonicalizes on ingest.

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

## What implementation changed

Amended 2026-08-17 after building this on `feat/node-identity-default-port`.

- **The wire format is not sequenceable.** The first draft said hub aggregates
  could be republished at leisure. They cannot: the agent renders `/graph.md`
  from a `graphstore`, so canonicalizing the store canonicalizes the export in
  the same commit. Accept-both stopped being a convenience and became the
  mechanism that makes the change survivable.
- **Injection was unnecessary.** See the decision section. One package-level
  function serves both transports because only one direction of normalization
  is safe to hardcode.
- **The etag coupling bit a second time.** `EtagFetcher` keyed etags by
  `"mark://" + host + path` with the dial port, so under the new rule they
  missed exactly as they had under the old one, silently turning conditional
  re-crawl back into a full refetch. Two different rules, same bug, because
  identity was being assembled by hand in more than one place. That is what
  motivated `links.NodeURL`.
- **The broker needed a matching change, and a subtle one.** Its seed
  translation keyed world prefixes on dial addresses with ports, so portless
  export rows stopped translating and every seeded row became unreachable.
  Canonicalizing both sides fixed it, but `CanonicalURL` normalizes an empty
  path to `/`, which broke the bare-authority prefix match until the trailing
  slash was trimmed. The cross-module contract test caught both.
- **Fedcrawl initially remained dial-keyed.** Explicit transport endpoints later
  gave it the ADR 0007 separation: crawl state, hash-index entries, and graph
  nodes use logical authorities, while socket addresses and TLS server names
  may differ. Default-port normalization still applies at the authority edge.

## Alternatives rejected

- **Keep the port always.** Internally consistent, and it is what the current
  data already looks like. Rejected because it leaves the natural spelling
  permanently wrong, keeps two identity rules across the two transports, and
  does nothing about absolute body links.
- **Normalize on read only.** Cheaper, and it would answer queries correctly
  today. Rejected because stored data stays forked on disk, so every export and
  every seed keeps propagating both spellings.
- **Hardcode the old rule inside `graphstore`.** Adding the default port in
  the store would have corrupted every brokered world key. Stripping it, the
  rule chosen here, is safe to hardcode because it is a no-op on a world name.
  The asymmetry is the point: normalization may only ever remove information
  that the scheme already implies.
- **Reject non-canonical input.** Honest, and it surfaces the problem instead
  of hiding it. Rejected because a store seeded from another world's `/graph.md`
  cannot control what that producer wrote, and refusing to seed is worse than
  normalizing.

## Deferred

- Whether the LOOKUP catalog and the OKF export should adopt the same identity
  rule. They key by path within a world today and are unaffected.
- A distinct `NodeURL` type so the compiler rejects a hand-built identity.
  `links.NodeURL` puts the rule in one place but cannot stop a future caller
  from concatenating a fifth one. Making it a named type touches every
  signature that carries a node URL, so it wants its own change.
- Case normalization of the authority. `mark://Host/x` and `mark://host/x`
  remain distinct until there is evidence anyone writes the former.
