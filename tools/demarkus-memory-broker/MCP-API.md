# Memory Broker MCP API

Operator + developer reference for the 12-tool MCP surface exposed by
the demarkus-memory-broker's `/mcp` endpoint. Eleven tools mirror the
local `client/cmd/demarkus-mcp` stdio server, scoped to the caller's
own world; `mark_worlds` is a broker-only self-discovery operation.
Transport, OAuth metadata, bearer handling, rate limiting, and the
write-token propagation retry are identical to the knowledge broker;
see `tools/demarkus-knowledge-broker/MCP-API.md` for those mechanics
and `deploy/helm/demarkus-memory-broker/README.md` for deployment.

## Identity + access model (differs from the knowledge broker)

Identity = world. The broker resolves the caller's canonical verified
email against the configured worlds' `allow` blocks and requires
exactly one match:

- Zero matches: every tool call is denied (`not authorized`).
- One match: that world is the caller's soul; reads AND writes are
  locked to it. Nothing is org-open.
- More than one match: a provisioning error; the broker denies closed
  with an opaque error and logs the colliding world names.

Cross-tenant access is denied before dispatch at three layers: the
tool-handler middleware (every world-bearing argument must address the
caller's world), MCP resource reads, and the graph crawler (a link
into a foreign world errors that node instead of fetching it). The
invariant is test-enforced across every registered tool
(`tools/internal/broker/mcp_tenant_test.go`).

## Tool surface (12)

Per-world parity tools, all locked to the caller's world:
`mark_fetch`, `mark_explore`, `mark_list`, `mark_versions`,
`mark_lookup`, `mark_publish`, `mark_append`, `mark_archive`,
`mark_discover`, `mark_backlinks`, `mark_graph`.

Broker-only: `mark_worlds` returns a one-row table naming the
caller's world (call it once to learn the `{worldName}` every other
tool addresses as `mark://{worldName}/{path}`).

Deliberately absent: `mark_lookup_all`, `mark_index`, `mark_resolve`
(federation / multi-world aggregation has no meaning for a
single-world tenant) and `mark_graph_export` / `mark_graph_publish`
(whole-store reads of the shared per-pod graph store would leak
cross-tenant edges).

## Soul seeding

A tenant world's first authorized tool call seeds the soul template
when `/index.md` is absent: `/.well-known/demarkus/policy.md`
(low-ceremony write policy: warn strictness, no required tag axes),
`/.well-known/demarkus/template.md` (the soul layout), then
`/index.md` last as the seeded sentinel. Publishes are create-only
and conflict-tolerant, so concurrent replicas converge; failures are
logged, throttled, and retried on a later call.

## Host guidance in the endpoint

The initialize result carries server `instructions` (recall-first,
record-as-you-go routing, metadata rules), and two prompts ship in
place of the knowledge broker's: `soul-context` (restore working
context from the soul) and `soul-journal` (append a dated journal
entry). Plugin-less hosts (Claude Desktop, ChatGPT, Cursor) get their
usage contract from these.
