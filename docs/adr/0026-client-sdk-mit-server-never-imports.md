# ADR 0026: A client SDK under MIT, which the server never imports

Status: proposed (2026-09-27). Phase 0 of the demarkus-sdk plan; settles the plan's license and versioning questions.

## Context

There is no client library. `client/fetch` is the de facto Go client: internal, AGPL-3.0-only, inside a module of 30+ packages that also holds the CLI, TUI and MCP code, with no stability promise. The only other client is a FETCH-only Java class embedded in a browser.

demarkus-library is MIT ([ADR 0009](0009-license-split-core-agpl-plugins-mit.md)) yet imports AGPL `client/fetch`, `protocol`, `client/mdoutline` and `client/graphstore`, so its binary carries AGPL obligations. Any third-party Go application hits the same wall. WATCH (ADR 0025) is only useful if applications can subscribe from their own code.

An authorship check on 2026-09-25, repeated on 2026-09-27 with `git log --format=%an` per file, shows one author (Fritz Seitz, under two author strings) for every file listed below. Git metadata is the inventory, not the proof of rights: the sole author holds the copyright, has accepted no contribution from anyone else and confirms in this ADR that they may relicense these files. A file that gains another author before the move needs that author's written consent or stays in core.

Two questions were open in the plan: MIT or Apache-2.0, and whether the SDK follows the monorepo's 1.0 lockstep version or its own semver.

## Decision

- **A client SDK and nothing else.** Repository `latebit-io/demarkus-sdk`, Go module `github.com/latebit-io/demarkus-sdk/go`, package `mark`. It opens a handle to a `mark://` host, speaks the eight verbs as typed calls, returns typed results and typed errors, pools QUIC connections, classifies connection versus stream failures, retries reads and never resends a write after bytes left the client. Response cache, recursive LIST walk, outline, links, merge, graph, token file discovery, MCP formatting, federation crawl and `client/live` stay in core on top of it.
- **MIT for the SDK, CC0-1.0 for wire fixtures.** MIT matches every satellite in ADR 0009 and is what Go adopters expect; the Apache-2.0 patent grant buys nothing for a single-author protocol client and would put a third license into the ecosystem. Wire fixtures are data, licensed like the specification.
- **The server never imports the SDK.** The SDK carries its own client-side codec: encode requests, decode responses and WATCH blocks. The server keeps the server-side codec in `protocol/`: decode requests, encode responses and blocks. The two directions agree through golden wire fixtures, CC0, that both repositories test against. A wire change without a fixture change is a review finding.
- **Core client surfaces import the SDK.** CLI, TUI, MCP, broker and agent switch one surface per PR with no behavior change; `client/fetch` is deleted once no import remains (plan phase 3).
- **The SDK has its own semver.** It is released from its own repository as v0.x until WATCH ships, then v1.0.0 with semantic versioning, so its API does not freeze before its largest addition. The monorepo's lockstep version (1.0 plan, W2) covers the monorepo modules only; core pins the SDK in `go.mod` like any dependency. Core 1.0 pins an SDK v1.
- **Realtime PR 1 lands in `client/fetch`.** Extraction had not started when the error classification split was written (2026-09-27), so it lands in core and moves with the file in phase 2.
- **Relicensed files.** Exactly these, all sole-authored; anything with another author stays in core.

  Moved to the SDK and deleted from core in phase 3:

  - `client/fetch/`: `budget.go`, `budget_test.go`, `cache_test.go`, `classify_test.go`, `conditional_test.go`, `evict_test.go`, `fetch.go`, `list_test.go`, `lookup_context_test.go`, `lookup_match_test.go`, `requests.go`, `stream_error_test.go`, `watch.go`, `watch_test.go`, `write_retry_test.go`
  - `client/listing/`: `listing.go`, `listing_test.go`
  - `client/lookuptable/`: `lookuptable.go`, `lookuptable_test.go`, `parse.go`, `parse_test.go`
  - `client/docwrite/`: `docwrite.go`, `docwrite_test.go`, in part: append version resolution and the look at the head after a lost response move; the merge candidate, which depends on `client/merge`, stays in core

  Copied into the SDK under MIT; the core copy stays AGPL for the server:

  - `protocol/`: `protocol.go`, `request.go`, `response.go`, `frontmatter.go`, `watch.go`, `frontmatter_test.go`, `request_test.go`, `response_test.go`, `watch_test.go`, `fuzz_test.go`, `metadata_test.go`, `reserved_test.go`, `hashpath_test.go`

  `protocol/watch.go` carries both directions of the WATCH block codec; the SDK copy keeps the decoder and the cursor type, the core copy keeps the encoder the server writes with. The WATCH wire forms pinned in `protocol/watch_test.go` become CC0 fixtures beside the existing goldens, so both codecs are tested against the same blocks.

  Relicensed to CC0-1.0 as fixtures:

  - `protocol/wiretest/testdata/*.golden`

  About 6,000 lines including tests.

## Consequences

- demarkus-library builds against the SDK after plan phase 4; its remaining AGPL imports are `client/mdoutline` and `client/graphstore`, which is the plan's open question, not this ADR's.
- Two codecs for two directions is deliberate duplication; the fixtures are the contract and the proof is that the SDK codec and the core server codec pass every fixture.
- `docs/adr/0009` still holds: the core stays AGPL; this ADR adds one MIT satellite and relicenses the listed files only.
- The SDK's own release cadence means a core release note names the SDK version it pins.
- Token file discovery (`~/.mark/tokens.d`) stays in core for now; if every application ends up resolving tokens the CLI's way, a later ADR moves it.
