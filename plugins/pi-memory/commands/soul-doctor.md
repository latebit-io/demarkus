---
description: Audit the soul (or a single project) for catalog hygiene — orphans, broken links, untagged docs, stale index entries, ADR gaps. Read-only.
argument-hint: "[project-slug | blank = whole local soul]"
---

# /soul-doctor

Run a read-only health check over a demarkus knowledge base and report what's rotting. This **never writes** — it surfaces findings and suggests fixes; the user decides what to act on.

## Scope

Resolve the audit root from `$ARGUMENTS`:

- **blank** → the whole local soul (`/`). Use the `demarkus-memory` MCP tools (`mark_graph` / `mark_list` / `mark_fetch` / `mark_backlinks`) with bare paths.
- **a project slug** (e.g. `demarkus`) → scope to `/<slug>/`.

Anchor on the scope's hub: `/index.md` or `/<slug>/index.md`.

(Auditing an organizational knowledge-system world is `/knowledge-doctor`'s territory in the demarkus-knowledge plugin, not this command.)

## Gather (cheap — two calls)

1. **Crawl the link graph.** `mark_graph` on the scope hub with `depth: 5`. Parse:
   - **Nodes:** `[status] <url> "title" N links` — note any status that isn't `ok` (e.g. `archived`), and `(no title)`.
   - **Edges:** `<from> -> <to>` — `mark://` targets are internal; `http(s)` targets are external.
2. **Inventory.** `mark_list` the scope (recurse into subdirectories) for the full set of documents that actually exist.

## Core checks (from the graph + inventory — no per-doc fetching)

- **Broken links** — an edge whose `mark://` target does not exist. The inventory (`mark_list`) is the source of truth for existence — **not** the crawl: a target can be absent from the graph's Nodes simply because it sat beyond the crawl depth, which is *not* a broken link. So flag a target only when it's missing from the inventory, and confirm each suspect with a single `mark_fetch` (`not-found`) before reporting `source → missing-target`.
- **Orphans** — an inventory document that is never the target of any `mark://` edge (no inbound links) and isn't the scope's root hub. It's unreachable except by knowing its path.
- **Stale index entries** — broken links whose source is a hub/index doc (an index pointing at something gone).
- **Missing hub** — a `/<project>/` subtree with documents but no `index.md`.
- **Untitled docs** — **`ok`** nodes shown as `(no title)` (no H1 / declared title). A `(no title)` on a `not-found` node is the broken-link finding above, not an untitled doc — don't double-report it.
- **ADR sequence** — for each `adr/` directory, list it and flag duplicate or gapped `NNNN` prefixes.

## Deep checks (per-doc `mark_fetch` — run on a small scope, or when asked)

These cost one fetch per document, so only run them for a single project or when the user asks for a thorough audit. **If you cap or sample, say so explicitly in the report — don't imply full coverage.**

- **Untagged docs** — fetch and check the `tags` metadata is non-empty. An untagged doc is invisible to `mark_lookup`. (This is what the publish gate now prevents going forward; this finds pre-existing ones.)
- **Duplicate content** — compare `content-hash` across fetched docs; identical hashes under different paths are duplicates.
- **Untyped docs (OKF `type`)** *(advisory)* — fetch and read the `type` metadata; a doc with no `type` or `type: Document` is un-kinded. Setting a `type` key in the `metadata` object (`Reference`/`Decision`/`Architecture`/`Plan`/`Journal`/`Guide`/…) makes the soul OKF-typed and filterable (`mark_lookup` `filter: type=…`). **Exempt `index.md` and `log.md`** — the server never defaults their type, so an untyped hub is correct. Advisory only: a local soul declares no `require_fields`, so this is a backfill suggestion, not a violation.
- **In-body frontmatter block** *(demarkus-specific)* — a body whose **first non-blank line is `---`** and whose block carries reserved/operational keys (`version`, `previous-hash`, `archived`, `meta.*`) is almost always an **exported demarkus doc pasted back into a publish**. The server stores frontmatter out-of-band and treats a body-leading `---` as literal content, so it renders as a stray horizontal rule + garbled headings, and the in-body `version:` won't match the doc's real fetched `version`. Flag it; fix is strip the block and re-publish with metadata passed out-of-band.
- **Dangling & unlinked references** — a relationship written in *prose* (or inline code) that the link graph never captured, because only `[text](url)` becomes an edge. A mention like "supersedes ADR 0005" is invisible to every graph-based check above. For each fetched body, scan for high-confidence reference patterns and resolve each against the **inventory** (existence) and the **doc's own parsed links** (already-linked?) — no extra fetches beyond the bodies this tier already pulls:
  - **Patterns** (keep the set tight — false positives in prose are worse than a missed edge):
    - ADR references — `ADR[ -]?#?\d{3,4}` (case-insensitive). Canonical target: an inventory path matching `<scope>/adr/NNNN-*.md`.
    - (Deferred to v2: bare/inline `mark://…` URLs not inside a link. They false-positive on cross-world refs whose existence a single-world audit can't check, and on inline-code placeholders like `` `mark://host/...` ``. Only attempt when you can scope the match to in-world hosts and exclude code spans.)
  - **Exclude**: the doc's *own* ADR number (self-reference), and any mention inside a fenced (```` ``` ````/`~~~`) code block — a `# ADR 0005` in a code sample is not a real reference (same fence-skip rule `firstH1`/the catalog use).
  - **Resolve "already-linked?" against the citing body's OWN markdown links — NOT the crawl's edge store.** The crawl seeds from the hub, so an **orphan** doc is never visited and its outbound links never enter the edge store; keying off edges would falsely flag an orphan's real `[ADR 0006](…)` link as unlinked. You already hold every body in this tier — parse each for its own `[text](url)` links and check whether one resolves to the mention's target.
  - **Classify** each surviving mention:
    - **Unlinked reference** — the target *exists* in the inventory but the citing body has **no markdown link** to it. The relationship is real and reachable but not traversable. Fix: convert the prose mention to `[ADR 0005](mark://…/0005-…md)`.
    - **Dangling reference** — the mention resolves to **no inventory doc** at all. It's referenced but absent (the ADR-0006-cites-a-missing-0005 case). Confirm with a single `mark_lookup` over the scope (catalog is authoritative for absence) before reporting. Fix: restore the doc, or correct/remove the reference. If the citing doc itself annotates the absence ("no md file exists"), report it as **known** rather than actionable.

  This is distinct from **Broken links** above: that check follows an *edge* to a missing target; this one finds references that were never edges in the first place.

## Report

Render plainly, grouped by check, most actionable first. For each finding give the path and a one-line suggested fix. Lead with a one-line summary (`N docs, X findings`). Shape:

```text
## <scope> — hygiene report  (<N> docs)

### Broken links (<n>)
- /<doc>.md → /<missing-target>.md  (fetch → not-found) — fix the link or restore the target

### Orphans (<n>)
- /plans/<name>.md — exists but no hub links to it; add it to the relevant index.md

### Untagged (<n>)        [deep check — scanned <k>/<N> docs]
- /<slug>/bar.md — no tags; re-publish with metadata.tags

### Dangling & unlinked references (<n>)        [deep check — scanned <k>/<N> docs]
- /adr/0006-navigation-rework.md → "ADR 0005" — dangling: no such doc in scope (lookup → no match); restore it or drop the reference
- /roadmap.md → "ADR 0006" — unlinked: target exists but no edge; convert the prose mention to a [link](mark://…)

### ADR / index / titles / duplicates …
```

End with a short prioritized "what I'd fix first." If everything is clean, say so plainly.

## Don't

- Don't write to the soul or modify any document. This command is read-only. If the user then asks you to fix something (e.g. "add the orphans to the index"), do that as a separate, explicit step.
- Don't fabricate. If `mark_graph` returns nothing (empty soul) or the scope hub is `not-found`, say so and stop.
- Don't fetch every document by default — the core checks don't need it. Only run the deep checks on a bounded scope, and disclose the bound.
