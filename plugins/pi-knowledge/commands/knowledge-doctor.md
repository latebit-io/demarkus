---
description: Audit an organizational demarkus knowledge system (broker-fronted, multi-world) for catalog hygiene — orphans, broken links, dangling/unlinked references, untagged + policy-axis-noncompliant docs, ADR gaps. Read-only.
argument-hint: "[slug | mark://<world>/ | blank = every joined system, all worlds]"
---

Run a read-only health check over an organizational demarkus knowledge system and report what's rotting. This is the knowledge-system counterpart to `/soul-doctor` (which audits the local soul). It **never writes** — it surfaces findings and suggests fixes; the user decides what to act on.

The substrate is identical to the soul's: the broker exposes the same `mark_*` tool surface remotely (`mark_worlds`, `mark_graph`, `mark_backlinks`, `mark_list`, `mark_lookup`, `mark_fetch`). Three things differ from `/soul-doctor`, and they shape every step below:

1. **Multi-world.** A system is many worlds; URLs are `mark://<world>/<path>`, not bare paths. Enumerate with `mark_worlds` and audit per world.
2. **The broker graph store is ephemeral** (per-pod lifetime; resets on broker restart). `mark_backlinks` only returns what *this* session has crawled — so you **must** `mark_graph` to populate it before any backlink/orphan reasoning, every run.
3. **Cross-world references are existence-checkable** here (they aren't in a single-world soul audit): `mark_worlds` + per-world `mark_lookup` can resolve `mark://<other-world>/<path>`. This un-defers the bare-`mark://` reference check (see Deep checks).

## Scope

Resolve the audit scope from `$ARGUMENTS`:

- **blank** → every joined system, all worlds. Read the registry for the joined systems:
  ```bash
  cat ~/.demarkus/knowledge-systems 2>/dev/null
  ```
  Each line is a system's MCP server slug; its tools are `<slug>_mark_*`. If the file is missing/empty, tell the user no knowledge system is joined (suggest `/knowledge-join <broker-url>`) and stop.
- **a slug** (e.g. `acme`) → that one system, all its worlds.
- **a `mark://<world>/` URL** → just that world.

For each system in scope, call `mark_worlds` to enumerate the worlds your identity may read (note the `writable` flag — a fix suggestion is only actionable on a world the user can publish to). Audit each readable world; anchor each on its `mark://<world>/index.md` hub.

> **Read-auth caveat.** `mark_lookup` is filtered to what your token may read, and `mark_worlds` lists only readable worlds. Your audit covers *your* visibility, not the whole system. Say so in the report — never imply full coverage of a world or system you can't fully see.

## Gather (per world — two calls each)

1. **Crawl the link graph.** `mark_graph` on `mark://<world>/index.md` with `depth: 5`. This also **populates the broker's ephemeral graph store** for this session — required before `mark_backlinks` returns anything. Parse:
   - **Nodes:** `[status] <url> "title" N links` — note any status that isn't `ok`, and `(no title)`.
   - **Edges:** `<from> -> <to>` — `mark://` targets are internal (possibly cross-world); `http(s)` are external.
2. **Inventory.** `mark_list mark://<world>/` (recurse into subdirectories) for the full set of documents that actually exist in the world.

## Core checks (from the graph + inventory — no per-doc fetching)

- **Broken links** — an edge whose `mark://` target does not exist. **Read the crawl-node status; it tells you what's already confirmed:**
  - A target **present in Nodes as `[not-found]`** is *already confirmed broken* — the crawler fetched it and the broker said not-found. Report it directly; **do not re-fetch** (re-confirming N dead links is N wasted calls).
  - A target **absent from Nodes entirely** may just have sat beyond crawl depth — *that* is the case the inventory (`mark_list`) settles. For a **same-world** target check the world's inventory; for a **cross-world** `mark://<other>/…` target check that world's inventory (or one `mark_lookup`/`mark_fetch`). Confirm before reporting.
  - A target present as **`[error]`** is not a missing doc — see the next check.
- **Unroutable / cross-system references** — an `[error]` node on a `mark://` link means the broker couldn't route the host. Classify by **`mark_worlds` membership**: if the host is **not a member world** (e.g. `mark://soul.demarkus.io/…` when the system is root+world-a), it's an **external/cross-system pointer** — report separately (intentional outbound link, or a stale reference to remove), not as a broken in-system doc. If the host **is** a member world, it's a **transient dispatch failure** — re-fetch once; only report if it still fails (don't flag a flake).
- **Orphans** — an inventory document that is never the target of any `mark://` edge (no inbound links) and isn't the world's root hub. Unreachable except by knowing its path. (Cross-world inbound links count — which is why you crawl every in-scope world before judging orphans.)
- **Stale index entries** — broken links whose source is a hub/index doc (`index.md`).
- **Missing hub** — a subtree with documents but no `index.md`.
- **Untitled docs** — **`[ok]`** nodes shown as `(no title)` (no H1 / declared title). A `(no title)` on a `not-found`/`error` node is the broken-link/cross-system finding above, **not** an untitled doc — don't double-report it here.
- **ADR sequence** — for each `adr/` directory, list it and flag duplicate or gapped `NNNN` prefixes.

## Deep checks (per-doc `mark_fetch` — run on a bounded scope, or when asked)

One fetch per document, so only run these for a single world (or when the user asks for a thorough audit). **If you cap or sample, say so explicitly in the report — don't imply full coverage.**

- **Untagged docs** — fetch and check `tags` is non-empty. An untagged doc is invisible to `mark_lookup`. (The publish gate prevents this going forward; this finds pre-existing ones.)
- **Policy-axis noncompliance** *(knowledge-only — no soul equivalent)* — fetch `mark://root/.well-known/demarkus/policy.md` once and read the **required tag axes** (e.g. `category:`) and any **`require_fields:`** (e.g. `type`). Then for each fetched doc, flag any that doesn't satisfy every required `axis:value` tag or required field. These docs were likely published before the axis/field was required, or via a `warn`-severity gate. Report with the missing axis/field and the policy's strictness (`warn`/`block`/`ask`).
- **Untyped docs (OKF `type`)** — fetch and read the `type` metadata; flag docs whose `type` is **missing or `Document`** (the generic default — un-kinded), so they can be typed to a real kind (`Reference`/`Decision`/`Architecture`/`Plan`/`Journal`/`Guide`/…). **Exempt `index.md` and `log.md`** — the server never defaults their type (`applyOKFTypeDefault` in `server/internal/handler/handler.go`), so a hub with no type is *correct*, not a finding; don't flag them. If the policy lists `type` under `require_fields:`, an untyped non-exempt doc is **noncompliance** (report with strictness, folds into the check above); otherwise it's an advisory **backfill candidate** — the bulk of any pre-OKF corpus, since the server types only docs published after the 0.18.0 upgrade. Fix: re-publish with an explicit `type` key in the `metadata` object.
- **In-body frontmatter block** *(demarkus-specific)* — a body whose **first non-blank line is `---`** and whose block carries reserved/operational keys (`version`, `previous-hash`, `archived`, `meta.*`) is almost always an **exported demarkus doc pasted back into a new publish**. The server stores frontmatter out-of-band and treats a body-leading `---` as literal content, so it renders as a stray horizontal rule + garbled headings, and the block's `version:` is stale (it won't match the doc's real fetched `version`). A real find from the world-a dry-run: `nib/index.md` opened with `version: 47` in-body while its stored version was 1. Flag it; fix is strip the block and re-publish with metadata passed as request metadata.
- **Style-guide violations** (`mark://root/.well-known/demarkus/style.md`) — for each fetched body, flag: **em dashes** anywhere in the body (the guide bans them; report a count per doc), and **duplicate headings** within one document (headings are `#section` anchors; a duplicate takes a `-1` suffix that shifts when sections move, silently breaking inbound anchor links; exclude `#` lines inside fenced code blocks). Fix: re-publish with unique headings and the em dashes replaced by a comma, colon, semicolon, or parentheses. The plugin's style gate (default `warn`, `DEMARKUS_STYLE_STRICTNESS` to adjust) catches these at write time going forward; this check finds the pre-existing corpus.
- **Duplicate content** — compare `content-hash` across fetched docs; identical hashes under different paths (even in different worlds) are duplicates.
- **Dangling & unlinked references** — a relationship written in *prose* (or inline code) that the link graph never captured, because only `[text](url)` becomes an edge. A mention like "supersedes ADR 0005" is invisible to every graph-based check above. For each fetched body, scan for high-confidence reference patterns and resolve each against the **inventory** (existence) and the **doc's own parsed links** (already-linked?) — no extra fetches beyond the bodies this tier already pulls:
  - **Patterns** (keep the set tight — false positives in prose are worse than a missed edge):
    - ADR references — `ADR[ -]?#?\d{3,4}` (case-insensitive). Canonical target: an inventory path matching `<world>/adr/NNNN-*.md`.
    - Bare/inline `mark://<world>/<path>` URLs **not** inside a `[text](…)` link — an intended reference that lost its link syntax. **Viable here** (unlike the soul audit): resolve the host against `mark_worlds`, then check that world's inventory/`mark_lookup` for the path. Still **exclude** inline-code placeholders (`` `mark://host/...` `` with ellipses) and fenced code blocks.
  - **Exclude**: the doc's *own* ADR number (self-reference), and any mention inside a fenced (```` ``` ````/`~~~`) code block — a `# ADR 0005` in a code sample is not a real reference (same fence-skip rule the catalog's `firstH1` uses).
  - **Resolve "already-linked?" against the citing body's OWN markdown links — NOT the crawl's edge store.** The crawl seeds from the hub, so an **orphan** doc is never visited and its outbound links never enter the (ephemeral) edge store; keying off edges would falsely flag an orphan's real `[…](…)` link as unlinked. This matters *more* on the broker, where the edge store is per-session. You already hold every body in this tier — parse each for its own `[text](url)` links and check whether one resolves to the mention's target.
  - **Classify** each surviving mention:
    - **Unlinked reference** — the target *exists* (same- or cross-world) but the citing body has **no markdown link** to it. Real and reachable, not traversable. Fix: convert the prose mention to `[ADR 0005](mark://<world>/adr/0005-…md)`.
    - **Dangling reference** — the mention resolves to **no doc** in any in-scope world. Referenced but absent. Confirm with a single `mark_lookup` against the candidate world (the catalog is authoritative for absence) before reporting. Fix: restore the doc, or correct/remove the reference. If the citing doc itself annotates the absence ("no md file exists"), report it as **known** rather than actionable.

  This is distinct from **Broken links** above: that follows an *edge* to a missing target; this finds references that were never edges in the first place.

## Report

Render plainly, grouped by world then by check, most actionable first. Lead with a one-line summary per system (`<system>: <W> worlds, <N> docs, <X> findings`). Shape:

```text
## <system> — hygiene report

### mark://<world>/  (<N> docs, writable: yes/no)   [coverage: your read scope]

#### Broken links (<n>)
- mark://<world>/<doc>.md → mark://<world>/<missing>.md  ([not-found] node — confirmed) — fix the link or restore the target

#### Cross-system / unroutable references (<n>)
- mark://<world>/index.md → mark://soul.demarkus.io/index.md  ([error], non-member host) — external pointer; remove or confirm intentional

#### Orphans (<n>)
- mark://<world>/<doc>.md — exists but no hub links to it; add it to index.md

#### In-body frontmatter (<n>)        [deep check — scanned <k>/<N> docs]
- mark://<world>/index.md — body opens with a `---` block (version: 47, stored version 1); strip it, re-publish with out-of-band metadata

#### Untyped — OKF type missing/Document (<n>)        [deep check — scanned <k>/<N> docs]
- mark://<world>/<doc>.md — type=Document (or none); set a `type` key in metadata, e.g. {"type":"Reference"}  (index.md/log.md exempt)

#### Policy-axis noncompliance (<n>)        [deep check — scanned <k>/<N> docs]
- mark://<world>/<doc>.md — missing required axis `category:` (policy strictness: block); re-publish with the axis tag

#### Dangling & unlinked references (<n>)        [deep check — scanned <k>/<N> docs]
- mark://<world>/adr/0006-….md → "ADR 0005" — dangling: no such doc in any in-scope world (lookup → no match); restore or drop the reference
- mark://<world>/architecture.md → "ADR 0006" — unlinked: target exists but no in-body link; convert the prose mention to a [link](mark://…)

#### Untagged / ADR / index / titles / duplicates …
```

End with a short prioritized "what I'd fix first." If everything is clean, say so plainly.

## Don't

- Don't write to any world or modify any document. This command is read-only. If the user then asks you to fix something, do it as a separate, explicit step — and only against a `writable` world.
- Don't skip the crawl. The broker's graph store is **ephemeral**; `mark_backlinks` is empty until `mark_graph` populates it this session. An empty backlink result after crawling is a real signal; an empty one *before* crawling is just an un-primed store.
- Don't fabricate. If `mark_worlds` returns nothing readable, a world's hub is `not-found`, or `~/.demarkus/knowledge-systems` is empty, say so and stop.
- Don't imply coverage you don't have. Read-auth scopes what you can see; disclose the bound (which worlds, how many docs, deep-check sample size).
- Don't confuse this with `/soul-doctor`. That audits the personal, local soul; this audits the shared, broker-fronted system(s).
