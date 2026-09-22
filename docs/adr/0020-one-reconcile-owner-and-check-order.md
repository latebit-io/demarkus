# ADR 0020: One reconcile owner, and arguments before authorization

Status: accepted (2026-09-21). Builds on [ADR 0019](0019-shared-tool-bodies-and-one-write-contract.md).

## Context

When the broker adopted `client/marktools`, two things the two MCP surfaces did
differently had to become one. The broker validated a write tool's arguments
and then authorized; the shared bodies authorized first, and `mark_index`
authorized after its crawl, so a caller who could never publish still cost a
full crawl. And a lost write response was handled in three places with three
rules: `merge.Candidate` looked at the head after any error, `docwrite` only
after `fetch.ErrOutcomeUnknown`, the generation publisher after any error, and
`mark_graph_publish` not at all.

## Decision

- Every write tool validates its arguments, then authorizes, then touches the
  network. The order lives in the tool body, so both surfaces have it. A dry run
  of `mark_index` publishes nothing and asks nobody.
- `client/docwrite` owns what happens after a write is sent, for PUBLISH in both
  conflict modes, APPEND and ARCHIVE: one `Result{Response, Reconciled,
  Candidate}`, one path that looks for a lost write. `client/merge` is the three way
  merge and the `on_conflict` parser, nothing else. `mark_graph_publish`
  publishes through `docwrite`.
- The server is asked only when the response was lost
  (`fetch.ErrOutcomeUnknown`). A write that never left cannot have landed, and a
  probe then only doubles the time a failure takes. A look that itself fails
  settles nothing and is reported beside the unknown outcome, never dropped.
- PUBLISH and APPEND are looked for at the version they would have created,
  `<path>/v<expected+1>`, which no later writer can change, so a head that has
  moved on does not turn a landed write into an unknown one. The write landed
  when that version holds every metadata key that was sent with the same
  value, surrounding whitespace aside, and the whole body matches: for PUBLISH
  the body sent, for APPEND the base version joined with the addition by the
  protocol's own rule (`storefmt.JoinContent`). A suffix is not enough: a
  competing write at the same version may end in the same words. A base that
  retention has pruned proves nothing, so nothing is claimed. ARCHIVE is a
  state of the head: it landed when the head is archived, whoever archived it.
  An archived document answers a FETCH without a version, so a reconciled
  ARCHIVE reports none; a version is given only when the probe learned one.
- An answered write is passed on as the server wrote it. Refusing one over
  malformed metadata would report a write that landed as a failure and invite
  the resend this contract exists to prevent. What a probe reads is validated,
  because its version decides whether a write landed.
- Generated documents (hash index, graph snapshot) keep their own rule in
  `client/generation`: settled by content, since every document is read back at
  its version anyway, and a refusal is settled the same way because the head
  may already be this exact document. It follows the lost response rule for
  errors. They do not go through `docwrite`, which would add a second, weaker
  check in front of the stronger one.

## Consequences

- The broker's tool handlers are argument parsing and hook bindings
  (`mcp_marktools.go`); about 1,400 lines of pasted bodies, the merge adapter,
  `indexWalk` and the second seed pass are gone. The seed pass is
  `graphstore.Store.Seed`, with a `Rewrite` hook where the gateway translates
  addresses and filters by tenant. Tenant gate, crawl world check and the seed
  filter stayed gateway code under their existing tests.
- Broker output changed where the owner accepted it: a lost response is
  reconciled or says the write may have landed, `mark_index` warns per skip and
  skips only a directory that is too deep, an incomplete siblings page is
  labelled `(first page)`, an unresolvable APPEND version reads
  `could not resolve version: ...`, and `page_size: null` reads as absent.
- Client output changed for a caller without a token who also sends bad
  arguments (the argument error comes first) and for `mark_index` without a
  token, which is refused before the crawl.
- In merge mode a failed publish still reads `publish failed: publish: ...`;
  the doubled word is kept because tool text is a contract. Fail mode never had
  it.
- The broker's index retry after a 401 wraps each document publish, not the
  whole generation.
