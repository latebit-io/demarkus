# Broker MCP API

Operator + developer reference for the 13-tool MCP surface exposed by
the demarkus-broker's `/mcp` endpoint. Mirrors the local
`client/cmd/demarkus-mcp` stdio server byte-for-byte: a document
fetched via the broker and the same document fetched direct-QUIC
produce identical body bytes, version, modified timestamp, etag, and
content-hash. The broker is a wire-shape adapter
(HTTPS/JSON-RPC ↔ QUIC/Mark Protocol), not a content transformation
layer.

For deployment instructions, TLS, and Ingress topology, see
`deploy/helm/demarkus-broker/README.md` § "MCP gateway".

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
  (`/.well-known/oauth-protected-resource`) and RFC 8414
  (`/.well-known/oauth-authorization-server`). An unauthenticated
  request to `/mcp` receives `401 + WWW-Authenticate: Bearer
  resource_metadata="…oauth-protected-resource"` per RFC 6750 + RFC
  9728, so a fresh client knows where to bootstrap.

## Identity + session model

The broker keys per-session state on the **canonical verified email**
extracted from the bearer (`email` claim, trimmed + lowercased,
`email_verified=true` required). This is the same identity dimension
the broker's `/me/install`, `/tokens` list/revoke, `AllowConfig`, and
audit logs already use — one identity per broker, no cross-surface
drift.

For each `(email, worldName)` pair, the broker lazy-mints a world
access-token on first use via `Issuer.MintFiltered`, caches it in
process memory, and re-uses it on subsequent calls. The cache:

- Is per-pod, in-memory only. Broker restart drops it; the next tool
  call re-mints.
- Has bounded size (`server.mcp.maxSessions`, default 10000) with LRU
  eviction.
- Times out idle sessions (`server.mcp.sessionMaxIdle`, default 1h).
- Coalesces concurrent first-call bursts on `(email, worldName)` via
  singleflight, so a plugin reconnecting after a broker restart
  triggers one mint per world, not N.

Raw world tokens are NEVER persisted to disk by the broker. They live
in memory; their hashes land in the per-world `tokens.toml` Secret
that the demarkus-server already manages.

## URL form

Every tool that addresses a world uses the URL shape:

```
mark://{worldName}/{path}
```

`{worldName}` is the logical name from the broker's `worlds[]` config
(typically the Kubernetes namespace / Service name). The broker
resolves the name to a cluster-internal address using
`worlds[].internalAddress` if set, otherwise the standard Service-DNS
convention `<name>.<namespace>.svc.cluster.local:6309`. The agent
never sees the internal address — it knows worlds only by name.

Path traversal, query strings, and fragments are rejected by the
gateway parser (strict shape, clear errors). The crawler used by
`mark_graph` is lenient about `?query` and `#fragment` on outbound
links it discovers in document bodies, because real-world markdown
embeds them; that leniency is crawler-scoped only, never the parser
for caller-supplied URLs.

## Tools

All 13 tools below have semantic parity with the local
`client/cmd/demarkus-mcp` stdio server. The proxy-fidelity gate is a
broker-side unit test that asserts the broker's `formatToolResult`
matches the local server's `formatResult` byte-for-byte across
representative `fetch.Result` cases.

### Read

#### `mark_fetch`

Fetch a document. Returns `status` (`ok` | `not-found` | `archived` |
`unauthorized`), `version`, `modified`, `etag`, `content-hash`, and
the markdown `body`. Identical wire shape to direct-QUIC `FETCH`.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_list`

List documents and subdirectories under a path. Returns a directory
listing as markdown.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_versions`

Version history of a document. Returns `total`, `current`, hash chain
validation status, and a list of versions with timestamps.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

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
candidates whose mint fails with `ErrNotAuthorized` — both collapse to
"this broker can't reach this candidate for me; try the next."

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `hash` | string | yes | `sha256-` + 64 lowercase hex chars. |
| `index` | string | yes | `mark://hub/index.md` or similar. |

### Federation writes

#### `mark_index`

Crawl a source world, collect content hashes, and publish a hash
index to a target world (typically a hub). Verifies the target has a
hub manifest unless `force: true`.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `source` | string | yes | Source world URL to crawl. |
| `target` | string | yes | Target URL where the index is published. |
| `expected_version` | number | no | Conflict detection on the target. `0` to create new. |
| `dry_run` | boolean | no | Return the index without publishing (default `false`). |
| `force` | boolean | no | Publish even if target has no hub manifest (default `false`). |

### Graph store (ephemeral)

> **The broker's graph store is in-memory only and per-pod-lifetime.**
> A broker restart drops every cached graph. Re-run `mark_graph` to
> re-populate. This is a deliberate trade-off documented under
> "Ephemeral graph store" in the chart README — bucket-store-backed
> persistence is parked for the post-broker design window.

#### `mark_backlinks`

Look up which documents link to a given URL, using the broker's
graph store. Returns results from previous `mark_graph` runs;
returns an empty hint if no crawl has populated the relevant edges.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |

#### `mark_graph`

Crawl outbound links from a document up to a depth and return the
link graph. External (non-`mark://`) links are recorded but not
followed. Results populate the broker's graph store for subsequent
`mark_backlinks` / `mark_graph_export` / `mark_graph_publish` calls.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | `mark://{worldName}/{path}` |
| `depth` | number | no | Default `2`, max `5`. |

#### `mark_graph_export`

Export the broker's graph store as a publishable markdown document
containing `mark://` links. Crawling the exported document naturally
re-discovers the topology. Returns an empty hint if the store is
empty.

(No parameters.)

#### `mark_graph_publish`

Export the broker's graph store and publish it to a world in one
step. `mark_graph_export` + `mark_publish` combined.

| Param | Type | Required | Notes |
| --- | --- | --- | --- |
| `url` | string | yes | Target where the graph document is published. |
| `expected_version` | number | yes | From a prior fetch. `0` to create new. |

## Error envelope

The broker returns errors via the MCP tool-error envelope (`isError:
true` in the `CallToolResult`) for genuine tool failures: malformed
URL, unknown world, transport failure to the world, exhausted
first-mint retry budget. World-side `conflict`, `archived`, and
`not-permitted` responses are forwarded **verbatim** with `isError:
false` — they are first-class outcomes the agent acts on (re-fetch
and retry; ask the operator for more scope), not gateway errors.

A few non-obvious categories worth pinning:

- **Unverified-email id_token** → `401` from `gatewayAuth` with the
  standard RFC 6750 + RFC 9728 `WWW-Authenticate` challenge. Same
  shape as a missing or expired bearer.
- **World-side `unauthorized` after a fresh mint** → retried by the
  broker with exponential backoff (`firstMint*` knobs) up to the
  configured attempts. The Kubelet projects refreshed Secret
  volumes on a sync cycle, so a just-minted token can briefly 401
  at the world before propagation completes. The retry loop is
  invisible to the agent.
- **Cache-hit `unauthorized`** (a token the broker had cached that
  the world now rejects: hand-revoked, sweeper-retired, etc.) →
  cache entry invalidated immediately and one re-mint attempted
  without backoff. Subsequent failures surface as the world's
  `unauthorized` envelope.
- **Cross-org candidate in `mark_resolve`** → the candidate is
  skipped with a logged reason; if all candidates skip or fail, the
  last failure surfaces in the tool response.
