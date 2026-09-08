# ADR 0012: Body match is an optional mode of LOOKUP

Status: proposed (2026-09-07). Workstream 2 of the token-efficient retrieval plan.

## Context

LOOKUP matches tags and titles only. The retrieval baseline of 2026-09-07 hit
27 of 27 vocabulary and decision questions and 0 of 23 body questions: the
term was in a section nobody tagged, so the agent fetched decoys, and the
decoy fetches are where the tokens went. The answer must name the section so
the next call is one section fetch.

Two homes were considered. A sidecar per client process would crawl every
world it serves, so read load scales with clients rather than content, every
answer lags the crawl, and two crawl loops need maintaining. The store already
holds an in-memory catalog rebuilt at load and updated on each write, and the
write path already has the full body in hand. A section index has exactly
that lifecycle.

The lookup plan kept full text out of core because ranking cannot be
specified across implementations. That constraint still holds and shapes the
contract.

## Decision

LOOKUP gains an optional request key `match` with values `catalog` (default)
and `body`. No new verb. Catalog mode is byte-identical to today: a request
without `match` gets the response it always got. When the request carries
`match`, the response echoes the mode it answered in; a server without body
match accepts `body`, answers in catalog mode, and echoes `match: catalog`,
while a server that predates the key answers with no echo. A client treats
any response lacking `match: body` as a catalog answer.

The spec fixes recall and leaves order to the implementation:

- The unit is the section, split and slugged by the one rule FETCH uses for
  `#anchor`. The splitter moves from `client/mdoutline` to
  `protocol/mdoutline` so server and client cannot disagree; the client
  package re-exports it.
- Section text is a heading's own content, not its subtree, so a hit names
  the innermost section.
- Terms are whitespace-split, lowercased, at least two characters, matched
  on alphanumeric token boundaries and as whole words.
- A document whose tags or title carry every term is one bare-path row, the
  catalog answer. Otherwise a section is a row when every term is in its
  text, its heading trail, or the document's tags or title, with at least
  one term in the text or trail itself. Every matching, readable row under
  the scope and filter is a candidate; `limit` truncates after ordering.
- Importance is a prior among matches and never admits a non-match. Ties
  break by path then anchor. Nothing else about order is specified. The
  reference server ranks by BM25 with heading and tag terms weighted higher,
  scaled by `0.7 + 0.3 * importance`.
- Body rows carry `#anchor` in the path, `title › heading` in the title, and
  a fifth `Snippet` column of one line at most 240 bytes.

The index lives in `server/internal/catalog` beside the catalog, in memory,
rebuilt with the catalog at load and updated per write. No persistence and no
library. Memory stays under three times body bytes. Servers that implement it
list `lookup-match: catalog, body` under `## Capabilities` in their agent
manifest, which is advisory: the manifest is author-published, so the
response echo is the authoritative signal.

## Consequences

- The `protocol` module gains a markdown parser (goldmark, already used by
  the client). Stripped server binaries grow by about 1 MB. The alternative,
  a second hand-rolled splitter, risks anchor drift against links already
  published, which is the failure the shared rule exists to prevent.
- The bucket store snapshot holds no bodies, so its section index reads the
  manifest, history, and body of every current document at open and of each
  changed document at refresh; unchanged documents carry their sections
  forward by body hash. Open indexes under the caller's context, not the
  per-request timeout. An on-disk index waits until a world passes about
  100 MB of bodies.
- `mark_lookup` on demarkus-mcp and the broker gains `match`;
  `mark_lookup_all` merges body rows; `demarkus lookup` gains `-match body`.
  No new MCP tool, so the schema budget is unchanged.
- Any pressure to specify ranking or snippet content in the spec is refused;
  the retrieval benchmark judges the reference implementation, not the spec.
