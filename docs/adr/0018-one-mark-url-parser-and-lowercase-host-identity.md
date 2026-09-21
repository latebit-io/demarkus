# ADR 0018: One mark URL parser, and host case is not identity

Status: accepted (2026-09-21). Amends [ADR 0005](0005-node-identity-default-port.md), which deferred case normalization of the authority.

## Context

A `mark://` URL was parsed in six places, each with its own rule.
`fetch.ParseMarkURL` lowercased the host, filled in the default port and
dropped query, fragment and userinfo. `links.CanonicalURL` stripped the default
port and kept the host as written. `index.canonicalServer` trimmed slashes,
cut a `:6309` suffix and lowercased the whole string. `index.validateEntry`
and the crawler's `normalizeServerURLs` each validated a server URL by hand,
port range included. The broker carried `brokerCrawlParseURL`, which read the
hostname as a world name without validating a port.

The rules disagreed where it mattered. The MCP server built node identity
from the parsed host, so `mark://Host/x` became the node `mark://host/x`; the
CLI and TUI canonicalized the raw string and kept `mark://Host/x`. One
document had two identities depending on the surface that crawled it.

`graph.Crawl`, `graphstore.CrawlAndPersist`, `Revalidate` and
`RevalidateBacklinks` took a `parseURL` func so the client and the broker
could each supply their own parser. The injection carried a second transport
rule, not only a dependency workaround: the client dials `host:port`, the
broker routes a world name.

`mark_index` and the federation crawler wrote servers into published indexes
by concatenating `mark://` and the dial address, so an index row, the manifest
`Source` line and the tool's result text all read `mark://host:6309`, while
the broker wrote the same server as `mark://world`.

## Decision

- `links.ParseMark` is the one parser. It returns a `Target` with the four
  forms callers need: `DialHost()` (lowercase `host:port`, default port filled
  in), `Hostname()` (as written, no port), `AuthorityURL()` and `NodeURL()`
  (identity). `links.ParseServer` is the strict form for a URL that names a
  server and nothing else. `CanonicalURL`, `NodeURL`, `AuthorityURL` and
  `DialHost` are conveniences over it. `fetch.ParseMarkURL`,
  `index.canonicalServer`'s own rule and both hand written server validators
  are gone.
- Identity lowercases the host. `mark://Host/x` and `mark://host/x` are one
  node. The path keeps its case. This follows RFC 3986 section 6.2.2.1 and
  matches what dialing, the token store and the response cache already did.
- No function takes a URL parser. `graph.Fetcher` and `graphstore.FetchFunc`
  receive the parsed `links.Target`; a direct client dials `DialHost()` and
  the broker routes `Hostname()`.
- A server is named by identity wherever it is published or shown: index rows,
  the manifest `Source`, tool result text, the crawler's normalized seeds and
  hubs. Build the name with `links.AuthorityURL`, never by concatenation.

## Consequences

- Stores migrate on their own. `CanonicalURL` already runs on load, ingest and
  read, so a graph store holding both host spellings merges them on load and
  keeps the more recently crawled copy, as it does for the two port spellings.
  No schema version bump.
- Published indexes change spelling from `mark://host:6309` to `mark://host`.
  Readers need no change: `index.Merge` compares servers by identity and
  `mark_resolve` parses either form. A new index replaces the rows an older
  client wrote for the same server.
- `Hostname()` stays as written because the broker matches world names case
  sensitively and does not restrict a configured name to lowercase. A world
  configured with uppercase letters keeps crawling, but its identity is
  lowercase, so its seed rows no longer translate back to the world. World
  names are lowercase everywhere today; rejecting an uppercase name at config
  load is left to the broker's move onto the shared package.
- A broker crawl that meets a URL with an invalid port now fails at the parse
  instead of at the fetch.
- The per server index document path the crawler publishes,
  `/index/<host:port>.md`, still carries the dial address. Renaming it moves
  published documents and is not part of this decision.
- The cache metadata `url` field stays a dial address. It is a label that
  nothing reads, and keeping it out of `links` keeps the transport package
  free of the markdown parser `links` links in.
- `joinurl` keeps its own host check. A join URL allows only DNS characters
  and no IPv6 literal, which is a narrower contract than a mark URL.

## Amendment (2026-09-21)

`Hostname()` is lowercase now, like every other identity accessor. The broker
was the reason it kept the host as written; since
[ADR 0021](0021-world-registry-names-and-one-tenant-door.md) a world name must
be a lowercase DNS label, and tool URLs treat it as case insensitive.
