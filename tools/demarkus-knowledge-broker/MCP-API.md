# Broker MCP API

Operator + developer reference for the 17-tool MCP surface exposed by
the demarkus-knowledge-broker's `/mcp` endpoint. Fifteen tools mirror the local
`client/cmd/demarkus-mcp` stdio server; `mark_worlds` and
`mark_lookup_all` are broker-only system operations. A document
fetched via the broker and the same document fetched direct-QUIC
produce identical body bytes, version, modified timestamp, etag, and
content-hash. The broker is a wire-shape adapter
(HTTPS/JSON-RPC ↔ QUIC/Mark Protocol), not a content transformation
layer.

For deployment instructions, TLS, and Ingress topology, see
`deploy/helm/demarkus-knowledge-broker/README.md` § "MCP gateway".

## Transport

- **Protocol**: MCP over Streamable HTTP (the MCP spec's preferred
  remote transport).
- **Endpoint**: `POST /mcp` on the broker's MCP listener (default
  `:8081`; typically fronted at `https://mcp.broker.<org>/mcp` by an
  Ingress).
- **Auth**: `Authorization: Bearer <id_token>` on every request.
  The broker accepts both broker-signed id_tokens (from the
  device-flow refresh grant, PR4) and IdP-signed id_tokens (from
  the device-flow completion, PR3). Unverified-email tokens are
  rejected at the gateway boundary.
- **OAuth metadata**: standard discovery via RFC 9728
  (`/.well-known/oauth-protected-resource`, bare and path-inserted
  `/mcp` forms, on the gateway listener) and RFC 8414
  (`/.well-known/oauth-authorization-server`, on the management
  listener; §3.3 requires the issuer's own origin). An
  unauthenticated request to `/mcp` receives `401 +
  WWW-Authenticate: Bearer
  resource_metadata="…oauth-protected-resource"` per RFC 6750 + RFC
  9728, so a fresh client knows where to bootstrap.

## Identity + access model

The broker derives identity from the **canonical verified email**
extracted from the bearer (`email` claim, trimmed + lowercased,
`email_verified=true` required). This is the same identity dimension
the broker's `/me/install`, `AllowConfig`, and audit logs already use:
one identity per broker, no cross-surface drift.

The broker does **not** mint per-user tokens. World access is gated as
follows:

- **Reads** (`mark_fetch`, `mark_list`, `mark_versions`, catalog lookups, and the
  federation reads) dispatch with an empty bearer. The world's
  `tokens.toml` grants no `read` operation to any token, so reads are
  open to any SSO-authenticated identity.
- **Writes** (`mark_publish`, `mark_append`, `mark_archive`, and the
  federation writes) dispatch with a single long-lived, per-world
  write token the broker provisions on first write and holds in
  process memory. SSO is the org gate; `WorldConfig.Allow` is the
  per-world writer allowlist enforced at the broker before dispatch.
  There is no per-user mint, no session cache, and no singleflight.

The per-world write token is shared across all authorized writers, so
the world only ever sees one token per world in its `tokens.toml`. Raw
tokens are never persisted by the broker except as the broker-namespace
per-world write-token Secret (the canonical record across pods); their
hashes land in the per-world `tokens.toml` Secret the demarkus-server
already manages. A broker restart drops the in-memory cache; the next
write re-reads the broker Secret rather than re-minting.

## URL form

Every tool that addresses one world uses the URL shape:

```text
mark://{worldName}/{path}
```

`{worldName}` is the logical name from the broker's `worlds[]` config
(typically the Kubernetes namespace / Service name). The broker
resolves the name to a cluster-internal address using
`worlds[].internalAddress` if set, otherwise the standard Service-DNS
convention `<name>.<namespace>.svc.cluster.local:6309`. The agent
addresses tools only by logical world name; internal addresses are not
accepted as tool authorities. The broker-only `mark_worlds` discovery
response deliberately exposes each internal topology address to authenticated
identities so graph consumers can join host-keyed edges to logical worlds.

Path traversal, query strings, and fragments are rejected by the
gateway parser (strict shape, clear errors). The crawler used by
`mark_graph` is lenient about `?query` and `#fragment` on outbound
links it discovers in document bodies, because real-world markdown
embeds them; that leniency is crawler-scoped only, never the parser
for caller-supplied URLs.

## Tools

Fifteen tools below have semantic parity with the local
`client/cmd/demarkus-mcp` stdio server. The proxy-fidelity gate is a
broker-side unit test that asserts the broker's `formatToolResult`
matches the local server's `formatResult` byte-for-byte across
representative `fetch.Result` cases.

### Read

#### `mark_worlds`

List every world the authenticated identity may read, including each
world's logical name, public URL, internal topology address, and
whether the identity may also write it. This is broker-only and takes
no parameters.

#### `mark_fetch`

Fetch a document. Returns `status` (`ok` | `not-found` | `archived` |
`unauthorized`), `version`, `modified`, `etag`, `content-hash`, and
the markdown `body`. Identical wire shape to direct-QUIC `FETCH`.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_explore`

Orient around one document in one call: outline head, outbound links,
recorded backlinks, and sibling documents, each capped at 10 entries.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_list`

List documents and subdirectories under a path. Returns one directory page as
markdown plus `complete` and, when incomplete, `next-cursor` metadata.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |
| `include_archived` | boolean | no | Include archived entries; default false. |
| `cursor` | string | no | Opaque `next-cursor` from the preceding page. |
| `page_size` | number | no | Maximum entries in this page, 1-1000. |

#### `mark_versions`

Version history of a document. Returns `total`, `current`, hash chain
validation status, and a list of versions with timestamps.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_lookup`

Look up documents by subject against the world's catalog. Matches the
query against each document's declared tags and title and returns an
importance-ranked markdown table of matches (path, importance, title,
tags), not document bodies. A catalog lookup, not full-text search:
a subject never tagged or titled is not found. Returns `matches` (the
row count). Dispatched unauthenticated like the other reads.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | Scope: `mark://{worldName}/` for the whole world, or a subtree like `mark://{worldName}/docs/`. |
| `query` | string | yes | Subject; matched against tags and titles (min 2 chars). |
| `filter` | string | no | Comma-separated `key=value`; built-ins `tag=`, `modified-after=`, `modified-before=`. |
| `limit` | number | no | Max results (server default 10, cap 1000). |

#### `mark_lookup_all`

Look up a subject across every readable world. The broker performs
bounded concurrent LOOKUP calls, qualifies each path as
`mark://{worldName}/{path}`, and applies one global result limit.
Results are ordered by per-world rank, then importance, world, and
path. Successful matches remain available when some worlds fail;
those failures are listed in a `status: partial` response. If every
world fails, the tool returns an error.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `query` | string | yes | Subject; matched against tags and titles (min 2 chars), or `*`. |
| `scope` | string | no | Server-relative scope applied to every world (default `/`). |
| `filter` | string | no | Comma-separated `key=value` predicates applied in every world. |
| `limit` | number | no | Global max results (default 10, cap 1000). |

### Write

#### `mark_publish`

Publish or update a document. Conflict-aware: on version mismatch,
the default `on_conflict: "merge"` returns a structurally-merged
candidate body (git-style conflict markers if both sides edited the
same lines) for the agent to verify, then re-call with
`expected_version` set to the candidate's `publish-at-version`. Pass
`on_conflict: "fail"` for the raw conflict envelope with no merge
attempt.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |
| `body` | string | yes | Markdown content. |
| `expected_version` | number | yes | From a prior fetch. `0` to create a new document. |
| `on_conflict` | string | no | `"merge"` (default) or `"fail"`. |
| `metadata` | object | no | Publisher metadata (`tags`, `importance`, opaque keys). Keys with the `rel-` prefix declare typed relations to other documents (`rel-supersedes: /adr/0002.md`, comma-separated for multiple targets); graph crawls ingest them as typed edges. |

#### `mark_append`

Append content to the end of an existing document. The server
concatenates after the existing body. `expected_version` is optional:
when omitted or `0`, the broker calls VERSIONS internally to resolve
the current version (one extra round trip, invisible to the agent).

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |
| `body` | string | yes | Markdown to append. |
| `expected_version` | number | no | Pass explicitly to skip the auto-resolve round trip. |

#### `mark_archive`

Archive a document. Archived documents return `archived` status on
FETCH; version history is preserved.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

### Federation reads

#### `mark_discover`

Fetch a world's agent manifest at `/.well-known/agent-manifest.md`.
Returns `not-found` if no manifest is published.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/` (or any path on the world; the broker resolves to the manifest path). |

#### `mark_resolve`

Resolve content by its SHA-256 hash using a hub index document.
Looks the hash up in the index, finds candidate servers, and fetches
by hash. Skips candidates the broker has no `worlds[]` entry for and
candidates the identity is not authorized for (`ErrNotAuthorized`);
both collapse to "this broker can't reach this candidate for me; try
the next."

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `hash` | string | yes | `sha256-` + 64 lowercase hex chars. |
| `index` | string | yes | `mark://hub/index.md` or similar. |

### Federation writes

#### `mark_index`

Crawl a source world, collect content hashes, and publish a hash
index to a target world (typically a hub). Verifies the target has a
hub manifest unless `force: true`.

The target path stores a `demarkus-hash-index/v2` manifest. The broker
stages version-pinned hash-prefix shards under the target's `.shards`
directory and compare-and-swaps the manifest last. Updates verify and
merge legacy or sharded target indexes before publication.
The manifest and its sibling `.shards` subtree must be publicly readable;
the target token requires publish access to both. Upgrade all readers and
writers before publishing v2 at an existing legacy path.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `source` | string | yes | Source world URL to crawl. |
| `target` | string | yes | Target URL where the index is published. |
| `expected_version` | number | no | Conflict detection on the target. `0` to create new. |
| `dry_run` | boolean | no | Return the index without publishing (default `false`). |
| `force` | boolean | no | Publish even if target has no hub manifest (default `false`). |

### Graph store (ephemeral)

> **The broker's graph store is in-memory only and pod-scoped.**
> Any event that recycles the broker pod (Helm rollout, OOMKill,
> node drain or eviction, kubelet restart) drops every cached
> graph. Re-run `mark_graph` against the relevant worlds to
> re-populate. This is a deliberate trade-off documented under
> "Ephemeral graph store" in the chart README; bucket-store-backed
> persistence is parked for the post-broker design window.
>
> To make restarts cheap (not durable), the store is **seeded on
> demand from the worlds' published graph**: any
> `mark_backlinks` / `mark_graph` / `mark_explore` call checks every
> configured world for `/graph/manifest.md`, loading its complete,
> version-pinned shards when present and falling back to `/graph.md`
> only when the manifest is `not-found`. Checks use the last seen etag,
> run at most once per world per 5 minutes, and merge successful loads
> into the store so cold pods answer backlinks without a crawl. Seed rows
> keyed by a configured world's internal dial address
> (the form the federation agent publishes) are translated to
> `mark://{worldName}/...` so they answer world-name queries. Locally
> crawled data always takes precedence over seeded rows, and a world
> without either graph form degrades to the unseeded behavior.

#### `mark_backlinks`

Look up which documents link to a given URL, using the broker's
graph store. Returns results from previous `mark_graph` runs and
the world's published graph seed; returns an empty hint if
neither has populated the relevant edges.
Each backlink carries its provenance (link label, source section
anchor, occurrence count) and typed relations like `[supersedes]`.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_graph`

Crawl outbound links from a document up to a depth and return the
link graph. External (non-`mark://`) links are recorded but not
followed. Edges carry provenance (link label, source section anchor,
occurrence count) and typed relations ingested from `rel-<predicate>`
publisher metadata. Results populate the broker's graph store for
subsequent `mark_backlinks` / `mark_graph_export` /
`mark_graph_publish` calls.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |
| `depth` | number | no | Default `2`, max `5`. |

#### `mark_graph_export`

Export the broker's graph store as a publishable legacy markdown document.
The Edges table carries six columns
(`From | To | Rel | Label | Anchor | Count`); parsers also accept the
legacy two-column `From | To` shape. Returns an empty hint if the store is empty.

(No parameters.)

#### `mark_graph_publish`

Export the broker's graph store and publish it to a world in one
step. `mark_graph_export` + `mark_publish` combined. This manual tool keeps the
legacy single-document contract; federation-agent snapshots are loaded
automatically from `/graph/manifest.md` when present. This tool does not seed
the store itself; run `mark_graph`, `mark_backlinks`, or `mark_explore` first.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | Target where the graph document is published. |
| `expected_version` | number | yes | From a prior fetch. `0` to create new. |

## Error envelope

The broker returns errors via the MCP tool-error envelope (`isError:
true` in the `CallToolResult`) for genuine tool failures: malformed
URL, unknown world, transport failure to the world, exhausted
first-mint retry budget, or failure of every world in `mark_lookup_all`.
Partial `mark_lookup_all` failures return `status: partial` with the
successful matches and an explicit failure table. World-side `conflict`, `archived`, and
`not-permitted` responses are forwarded **verbatim** with `isError:
false`; they are first-class outcomes the agent acts on (re-fetch
and retry; ask the operator for more scope), not gateway errors.

A few non-obvious categories worth pinning:

- **Unverified-email id_token** → `401` from `gatewayAuth` with the
  standard RFC 6750 + RFC 9728 `WWW-Authenticate` challenge. Same
  shape as a missing or expired bearer.
- **World-side `unauthorized` after first provision** → retried by
  the broker with exponential backoff (`firstMint*` knobs) up to the
  configured attempts, against the same long-lived per-world write
  token. The Kubelet projects refreshed Secret volumes on a sync
  cycle, so a just-provisioned token can briefly 401 at the world
  before propagation completes. The retry loop is invisible to the
  agent; once the retry budget is exhausted the world's
  `unauthorized` envelope surfaces.
- **Cross-org candidate in `mark_resolve`** → the candidate is
  skipped with a logged reason; if all candidates skip or fail, the
  last failure surfaces in the tool response.
