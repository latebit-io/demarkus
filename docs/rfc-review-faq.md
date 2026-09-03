# Demarkus / Knowledge System FAQ

For the RFC review session. Sources: demarkus repo, demarkus-knowledge-system-deploy repo. Status: WIP.

## How do you interact with the knowledge system?

Two access paths: MCP over HTTPS for agents, web or CLI for humans.

- Agents: `mark_*` MCP tools against the broker over HTTPS, URLs as `mark://<world>/<path>`.
- Auth: the broker is its own authorization server. Claude Code drives OAuth authorization code with PKCE and dynamic client registration; device flow is also supported, for CLI-style joins.
- Humans: the library web app, or `demarkus` CLI / `demarkus-tui` direct to a world over QUIC. The library reads one world over QUIC with no login by default; pointing it at the broker as a registered web client is what adds sign-in and browser writes.
- Navigation anchors on the `root` hub; policy and templates under `mark://root/.well-known/demarkus/`.

## What are a world, a hub, root, and an index?

- **World**: one demarkus server in a knowledge system, addressed by logical name. Own namespace, store, tokens, and release. It may describe itself in an optional `world.md`; a world without one still works.
- **Hub**: an ordinary world that receives published aggregates, the content-hash indexes from `mark_index` and the graph exports (`/graph/manifest.md` snapshots plus legacy `/graph.md`). `mark_resolve` and backlink seeding read from it. Not a protocol or server concept: the server binary has no hub mode, and `hub: true` is a deploy-repo flag the crawler reads.
- **Root**: the conventional name for the hub world. Not guaranteed by the protocol. The deploy only checks that some world sets `hub: true`, but the name `root` is hardcoded in the secret store and library config, so in practice the hub must be called root.
- **Index**: `index.md` is a curated entry-point document, the discovery backstop for what lookup can't surface. The crawler writes machine hash indexes to `/index/<host>.md` when `perServer: true` (the chart default); setting it false writes `/index.md` and overwrites the curated one. Large indexes publish as a sharded v2 manifest with shards under `<manifest>.shards/` in alternating a/b slots, manifest committed last so readers never see a partial index.

## What automated cues push agents to record memory?

Five hooks in the memory plugin.

- SessionStart: injects the routing table (decisions to `/adr/`, gotchas to `debugging.md`, progress to `journal/`).
- Stop: journal nudge when files changed but nothing was written to the memory.
- PostToolUse on `mark_publish`: promote nudge when an ADR is published to the memory and a promote destination is configured.
- UserPromptSubmit: recall-first reminder on "did we decide" questions.
- Pre/PostToolUse write gate: checks tags and importance, plus write destination, retention, and style. Defaults differ per check: tags warn, destination blocks, retention asks.

## How does content get into the knowledge system?

Through the promote bridge: `/promote <memory-path>` runs the `knowledge-promote` cascade.

- Triage → distill for a shared audience (strip personal framing, secrets, PII).
- Dedup against the catalog → tag to the system taxonomy → route to a writable world.
- Human gate → `mark_publish` with provenance. The cascade never writes the memory; `/promote` back-stamps the source afterwards.
- Direct `mark_publish` to a joined system also works; it passes the same write gate and policy.

## How does finding knowledge work?

Primary: `mark_lookup` catalog queries. Secondary: graph traversal and hub pages.

- Lookup matches a query against declared tags and titles; returns an importance-ranked table (path, importance, title, tags).
- Filters: `tag=`, `modified-after=`, `modified-before=`, plus any `key=value` matching a declared metadata value exactly. Not full-text search; untagged documents are invisible to it.
- From a hit: `mark_explore` to orient, `mark_fetch url#anchor` for the sections needed.
- Backstops: `index.md` hubs, `mark_backlinks` / `mark_graph` for link traversal, `mark_discover` for a server's manifest.

## How is relevance decided?

Deterministic scoring, no embeddings (`server/internal/catalog/catalog.go`).

- Score = count of distinct query terms matching tags (exact, case-insensitive) or title (substring, case-insensitive).
- Sort: score desc, then declared `importance`, then modification time, then path.
- Filters apply before ranking.

## How is importance stamped on a document?

The publisher declares it.

- `metadata.importance` on `mark_publish`, float in [0,1], stored out of band, indexed into the catalog.
- Absent, unparseable, or out-of-range values default to 0.5.
- APPEND merges: a new version carries the base version's metadata with the request's layered over it (SPEC §6.6), so importance and tags survive an append. `retention` is the exception and is never inherited. Servers predating that merge dropped both, which is why some older documents sit at 0.5 with no tags; re-publish the full body with the recovered metadata to repair one.

## How does the agent decide importance?

The agent chooses it, following guidance injected at session start. Nothing computes it.

- SessionStart context and the `memory` / `knowledge-promote` skills instruct: reserve 0.8+ for hubs, architecture, key decisions; routine notes lower.
- The server never infers it; the gate only validates the range.

## How does the document graph work?

`mark_graph` crawls outbound `mark://` links (depth default 2, max 5, capped at 200 nodes,
so a deep crawl usually stops on the node cap rather than the depth limit).

- Edges carry provenance: link label, source section anchor, occurrence count.
- Typed relations come from `rel-<predicate>` publisher metadata, e.g. `rel-supersedes`.
- Crawls persist to a graph store that answers `mark_backlinks`; seeded from a verified `/graph/manifest.md` snapshot with `/graph.md` fallback, local crawls take precedence.
- `mark_graph_publish` republishes the store as a crawlable `/graph.md` (the legacy single-document form; generated doc, default retention 20). Version-pinned sharded snapshots behind `/graph/manifest.md` are published by the federation agent.

## How is the graph built world to world?

A scheduled agent builds it; the broker reads what that agent publishes.

- The agent is configured with a seed URL per content world and crawls each one. The hub is not crawled; it is where results are published.
- It merges every world's edges into one graph, publishes an atomic `/graph/manifest.md` snapshot, then updates compatible `/graph.md` on the hub alongside the content-hash index. Graph publishing is off by default and switched on in this deployment.
- Each run rebuilds from scratch, so a world that fails to answer is skipped and drops out of the published graph until the next successful crawl.
- Cross-world links are ordinary `mark://{worldName}/{path}` links, so an edge from one world to another is just a link the crawl followed.
- The broker's own graph store is in-memory and pod-scoped. It loads the hub's verified snapshot, or legacy `/graph.md`, on demand so a fresh pod answers backlinks without crawling.
- A world with no published graph falls back to on-demand `mark_graph` crawls only.

## Is edge information there to guide agents to relevant information?

Yes (ADR 0004).

- Label: what the linker calls the target.
- Anchor: the exact source section, a direct `mark_fetch url#anchor` jump.
- Count: link strength signal.
- `rel-` types: "what superseded this" rather than bare "mentions".
- ADR 0004 constraint: the agent owns judgment, the server owns accumulation.

## What does the agent use to decide whether to read a document?

Catalog and graph evidence before bodies.

- The `mark_lookup` row: path, importance, title, tags, modified time.
- Backlink provenance: label, anchor, count, `rel-` type.
- `mark_explore`: outline, links, backlinks, siblings in one call.
- Staged reading: `mark_fetch` returns an outline once a body reaches 8KB, so the agent targets a `#section` anchor.
- `mark_discover` manifests and hub `index.md` pages set context first.

## Why deploy a knowledge system on k8s?

A knowledge system is distributed: many logically isolated worlds behind one broker.

- At production scale, one multi-replica `demarkus-knowledge-server` Deployment hosts every world: TLS SNI selects the world per connection, and each world keeps its own GCS bucket (with an immutable world ID), tokens, policy, and rate limits. The earlier shape, one StatefulSet per world, is superseded by this.
- A world is an independent failure domain for its own content: its bucket, tokens, and policy are its own, and other worlds keep serving and answering lookups.
- A world being down degrades shared state: the next crawl silently omits its nodes and edges from the hub graph, so cross-world backlinks for it disappear until a later successful crawl. If the down world is the hub, cross-world discovery goes with it.
- Adding a world is one entry in the server's `worlds[]` config plus a bucket bootstrap; no new workload.
- Replicas are stateless over shared GCS; writes race on a compare-and-swap of one head object per world, with no leader election. The chart refuses fewer than 2 replicas.
- The world list and deployment identity are declared once and reconciled by ArgoCD. Versions, secrets, bootstrap, and the hub name are not.
- Smaller needs: the reference deployment's README calls this overkill for teams and says one or two plain deploys would do.

## How does the system keep data fresh?

- Reads go to the world's server every time. The client revalidates with `if-none-match` / `if-modified-since`, so a cached body is only reused when the server confirms it.
- Each write creates a new hash-chained version. Re-writing identical content is a no-op that returns the existing version rather than a new one.
- Optimistic concurrency (`expected_version` plus merge-on-conflict) stops stale writes clobbering newer ones.
- A scheduled agent re-crawls every world and republishes the content-hash index and graph on the hub.
- Memory copies of promoted documents can go stale. Refresh runs one way only: `/memory-refresh` copies the knowledge system's version down to the memory, and `/promote` is the only way a memory edit goes back up. There is no two-way sync.

## How do diffs and merges work?

This is about one document with two concurrent editors. Merging separate documents is
dedup, below.

- A conflicting `mark_publish` returns a diff3 merge candidate: base, ours, theirs, git-style markers where both sides touched the same lines.
- The machine merges structure; the agent reviews the result semantically and republishes at the returned version.
- Effect: a stale body cannot silently overwrite a newer one.

## How are documents updated, and how do they keep their meaning?

- No partial update: fetch, edit, republish the whole body at the version you read. Conflicts return a merge candidate instead of overwriting.
- Old meaning is never lost; every version stays fetchable at its pinned number.
- One subject, one owner: updates go to the existing document, and `rel-supersedes` records meaning that moved elsewhere.
- Limit: nothing verifies an update preserved meaning. That is agent judgment plus the human gate, and renaming a heading silently breaks inbound anchors.

## How does dedup work?

Dedup happens before the write, not as a cleanup pass. Nothing detects meaning
automatically; the agent looks for an existing owner of the subject and updates it.

- Before publishing, the promote cascade looks the subject up in the catalog and fetches the close matches.
- If a document already covers it, the agent updates that document instead of adding a second one.
- If the new content contradicts the existing one, that goes to the human at the gate rather than being merged.
- `/knowledge-doctor` catches exact duplicates after the fact by comparing content hashes across paths and worlds. It fetches each document to do so, so it runs on a bounded scope, not the whole corpus.
- Limit: the agent compares candidate bodies and judges them, but it finds those candidates by tag and title match. A near-duplicate sharing no tags or title terms never becomes a candidate.

## Why does federation matter, and why a server per world or memory?

The protocol has no central authority or registry. Note the deployment does not inherit
that property.

- Anyone can run a server; content mirrors freely; every client that caches is a mirror.
- Each version carries a `content-hash`, which is what lets agents on different mirrors confirm they hold identical content. The hash chain is a separate guarantee: it proves one server has not rewritten its own history. VERSIONS returns no per-version hashes, so the chain is not a cross-mirror check.
- A server per world makes the ownership boundary physical: own store, token file, writer allowlist, release. Blast radius stays contained.
- A memory is the same `demarkus-server` binary at personal scale.
- `mark_resolve` fetches by hash via a hub index, across the worlds the broker is configured for. Cross-org resolution is explicitly out of scope today.
- Worth saying plainly at review: this deployment is centrally administered by design, with one broker, an OIDC org gate, a domain allowlist, and per-world writer allowlists. The protocol is decentralized; this instance is not.

## What tools does the memory plugin expose?

Fifteen `mark_*` MCP tools.

| Tool | Description |
|------|-------------|
| `mark_fetch` | Fetch a document or `#section`; bodies of 8KB or more return an outline |
| `mark_list` | List one directory page; follow `next-cursor` until `complete`; archived hidden by default |
| `mark_explore` | One doc's outline, outbound links, backlinks, and siblings in one call |
| `mark_lookup` | Catalog lookup: importance-ranked matches on tags and titles |
| `mark_publish` | Create or update a document; metadata, optimistic concurrency, diff3 on conflict |
| `mark_append` | Append to an existing document; the server carries its catalog metadata forward |
| `mark_archive` | Archive a document; body and catalog entry withdrawn, history preserved |
| `mark_versions` | Version history with hash-chain validation |
| `mark_graph` | Crawl outbound `mark://` links; persists edges to the graph store |
| `mark_backlinks` | What links here, with edge provenance |
| `mark_graph_export` | Export the graph store as publishable markdown |
| `mark_graph_publish` | Export and publish the graph as legacy `/graph.md` (retention default 20); sharded snapshots are the agent's job |
| `mark_discover` | Fetch a server's agent manifest |
| `mark_resolve` | Resolve content by SHA-256 hash via a hub index |
| `mark_index` | Crawl a server, publish its content-hash index to a hub |

## What tools does the knowledge plugin expose?

The same 15 MCP tools as the memory plugin, kept at parity so the vocabulary does not
change with transport. URLs address worlds by logical name instead of host:port. On top
of those:

| Addition | Kind | Description |
|----------|------|-------------|
| `mark_worlds` | MCP tool | List the system's worlds: name, URL, address, and whether you can write to each. Broker-only, since the local MCP's universe is its one world |
| `mark_lookup_all` | MCP tool | Universe-wide catalog lookup: one query fanned out to every readable world, merged into a single globally limited table with `partial` status on per-world failures. Broker-only |
| `/knowledge-join` | command | Validate an org broker URL, register it as an MCP server, and copy its policy into the local gates as a snapshot. Claude Code itself runs the OAuth flow on the first tool call; the command handles no tokens |
| `/knowledge` | command | List joined systems and show each `root` hub index |
| `/knowledge-doctor` | command | Read-only hygiene audit: orphans, broken links, untagged and policy-noncompliant docs, ADR gaps, duplicates |
| `knowledge-promote` | skill | The curation cascade that lands a staged document in the catalog; invoked by the memory plugin's `/promote` |

With the two broker-only tools the brokered surface is 17 MCP tools. Writes pass a per-world writer allowlist with one shared per-world token; SSO is the org gate.

## How is knowledge formatted and held to a standard?

The standard is itself published on `root` under `.well-known/demarkus/`.

- `policy.md`: strictness, required tag axes such as `category:`, optional required OKF fields.
- `template.md`: per-world layout.
- `style.md`: H1 as name, one-sentence summary under it, unique headings (headings are anchors), no em dashes, no frontmatter fences.
- Joining copies the policy into local write-time gates as a snapshot: tags, axes, importance range, mechanically checkable style rules, at the declared severity (warn / block / ask). The gate runs offline and cannot reach the broker, so a policy change takes effect on the next join or mirror.
- `/knowledge-doctor` audits the corpus after the fact.

## Why QUIC?

- Encryption is mandatory: TLS 1.3 built in, no plaintext fallback.
- Fast multiplexed streams, one per request.
- No HTTP layer: no cookies, tracking headers, or query strings. Seven text verbs instead.
- The broker fronts worlds over HTTPS, so clients need no direct QUIC access.

## What is the auth and security model?

Capability tokens on the server; SSO at the broker.

- Writes are denied unless a token store is configured.
- Reads are public unless a token grants `read` on a path pattern, which makes matching paths require one (protect `/**` for a private intranet).
- Knowledge system: OIDC SSO is the org gate. Reads open to any authenticated identity; writes pass a per-world writer allowlist, dispatched with one shared long-lived per-world token.
- Policy forbids publishing secrets, credentials, or PII.

## How are versions and integrity handled?

- Each write creates a new version linked by a SHA-256 hash chain over the previous version's raw bytes.
- `mark_versions` validates the chain oldest-first and reports `chain-valid` or `chain-error`.
- Nothing is deleted by default. `mark_archive` keeps full history, but it does more than hide: the document stops returning a body at its path, drops out of the lookup catalog, and rejects further writes until it is unarchived by publishing an empty body.
- The one destructive path is `metadata.retention`, which permanently prunes old versions. It exists for generated documents; clients warn before applying it elsewhere.

## Why no full-text or semantic search?

Deliberate. The LOOKUP verb is defined in SPEC §6.7; the argument for keeping search out
of core is in DESIGN.md under "Why not search?".

- LOOKUP is a per-world catalog over author-declared tags and titles. Servers must not read document bodies at query time, and there is no centralized index.
- Full-text and semantic search stay out of core as an opt-in sidecar reading demarkus over LIST/FETCH.
- Keeps the server simple and deterministic; ranking judgment stays in the agent.

## What are the known limitations?

- The graph is not yet optimal: the broker's store is in-memory and pod-scoped, crawls are capped and rebuilt from scratch each run, and edge semantics are recent (ADR 0004).
- Full-text or semantic search requires an added component; until then recall depends on tagging discipline.
- Availability is partly distributed: the broker defaults to 2 replicas with a PodDisruptionBudget, and the knowledge server requires 2 or more stateless replicas over shared GCS; the hub remains a single point for cross-world discovery, and the broker's graph store is still pod-scoped.
- Writes inside a world share one token, so per-user attribution rests on the provenance recorded in documents, not on the credential.
- Legacy servers replaced a document's metadata on append rather than merging it, so documents appended before the merge landed may still be missing tags.
- Behavior at large scale is unproven: catalog size, crawl cost, and cross-world graph growth have not been tested at enterprise corpus sizes.
