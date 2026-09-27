# ADR 0025: WATCH is the eighth verb, and the store emits hints only

Status: proposed (2026-09-27). Realtime PR 2; supersedes the "Verb Set: Complete" rule in the architecture notes.

## Context

The verb set was declared complete at seven after LOOKUP. Every consumer that needs to know about a change polls: the MCP response cache waits out a TTL, the library re-reads on navigation, an agent mailbox would poll its inbox. Polling puts a floor under latency equal to the interval and costs a request per interval per consumer.

The realtime plan weighed three levels: live changes (a write reaches every watcher, cache and open view within a budget), live views (LOOKUP results, graph neighbourhoods and library panes update themselves), and live collaboration (concurrent editing, presence, cursors). The first two are what makes knowledge reactive. The third is session state.

The earlier WebSub-style `SUBSCRIBE` with a callback URL was removed because agents are not servers. QUIC gives the alternative: a client-opened stream that stays open, many on one connection without head-of-line blocking, cancelled by stream reset, kept alive across network changes.

The 1.0 plan left open whether protocol 1.0 freezes before or after WATCH and the protocol version in ALPN land.

## Decision

- **WATCH is the eighth verb.** `WATCH <path>` subscribes to change hints for one document or every document under a directory prefix; `since: <cursor>` resumes. The response is a stream of frontmatter blocks with no bodies: an acknowledgement, events, heartbeats, then one terminal block. The set is eight; a ninth needs an ADR of its own.
- **Levels 1 and 2 are in scope; level 3 stays out of the store.** Concurrent editing, presence and cursors belong to a separate room service that checkpoints through PUBLISH. The event format leaves room: a client ignores keys and operations it does not know.
- **The store emits hints and nothing more.** An event names `path`, `version`, `hash`, `op` and `agent`; a client fetches content by hash. The server keeps no durable subscriber state, a watch lives as long as its stream, overload ends a slow subscriber with `resync`, and a drain ends every watch with `closing`. This is what keeps subscriptions compatible with the no-sessions rule.
- **Cursor and delivery.** A cursor is opaque `<epoch>:<seq>`; the epoch changes when the sequence restarts. Delivery is at least once and ordered within an epoch. `resync` means the client rebuilds from LIST, VERSIONS and FETCH and resumes from the returned cursor. A client never infers a gap from cursor values; only `resync` signals one.
- **Statuses `resync` and `closing` are added**; `unauthorized` is also a terminal block when a token is revoked mid-stream. Errors before subscription are ordinary responses in place of the acknowledgement.
- **Wire limits.** One block is at most 8192 bytes, fences included; block values follow the metadata grammar. Heartbeats every 20 seconds while idle. Codec in `protocol/` (wire and stored format, ADR 0014): `VerbWatch`, `Cursor`, `WatchBlock`, `WatchReader`, `WatchEvent`; the client-side reader is copied into the SDK (ADR 0026).
- **Additive, no version bump.** A server that predates WATCH rejects the verb at the parser and answers `bad-request`, never a hang; the protocol version in ALPN stays its own workstream. Proof: a handler test sends WATCH and gets `bad-request`.
- **Read authorisation applies to WATCH** on the prefix at subscription and to every event before it is sent; a token reload rechecks open watches.
- **Protocol 1.0 freezes after WATCH and ALPN `mark/1` land.** The specification is not frozen and extended in one cycle. Until both land, the 1.0 plan's item 2 (protocol final) stays open and the spec version stays pre-1.0.

## Consequences

- SPEC gains §6.8 and the statuses, request and response fields, size limit and constants that go with it; the Subscriptions entry leaves §13. `docs/DESIGN.md` points from the removed WebSub sketch to §6.8.
- The store serves WATCH through a change hub (`server/internal/changefeed`): a ring of hints, read by each subscriber at its own pace, with a lapped subscriber told to resync. Both stores publish their own commits to it under their commit lock, so hints follow commit order; the bucket store also, while a world has watchers, polls the bucket head every 5 s so a replica's watchers see the other replica's writes (an append seen that way is reported as `publish`). Watches are admitted under their own caps rather than the request concurrency slots, are exempt from the request deadline, and are capped per world and per connection below the stream limit; a shutdown ends them with `closing` before the listener drains. The client (`client/fetch.Watch`, `demarkus watch`) reconnects from its cursor and surfaces resync. A server without a hub still answers `bad-request`.
- Not yet: commit hints pushed between replicas (the `mark-peer` ALPN and its chart wiring), so cross-replica latency is bounded by the poll, not the 500 ms budget; and a durable epoch, so a restart or a move to the other replica costs one `resync`.
- No MCP tool is added; the MCP feature freeze is not triggered. Agents get updates through MCP resource notifications later.
- The store now holds open streams. The risk is drift toward a message bus: every mailbox message is a stored version, and chatter belongs elsewhere. The mailbox skill says so.
- A dropped event would look like quiet. Cursors on heartbeats, contiguous sequences inside the change hub, and `resync` on any gap keep silence honest.
- Every MCP session, library and SDK application will hold a watch. Worlds see one watch per QUIC broker replica once fan-out lands; until then, per-world and per-connection limits below the stream limit bound the cost.
