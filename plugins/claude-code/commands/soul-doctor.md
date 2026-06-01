---
description: Audit the soul (or a project / knowledge-system world) for catalog hygiene — orphans, broken links, untagged docs, stale index entries, ADR gaps. Read-only.
argument-hint: "[project-slug | mark://<world>/ | blank = whole local soul]"
---

Run a read-only health check over a demarkus knowledge base and report what's rotting. This **never writes** — it surfaces findings and suggests fixes; the user decides what to act on.

## Scope

Resolve the audit root from `$ARGUMENTS`:

- **blank** → the whole local soul (`/`). Use the `demarkus-memory` MCP tools (`mark_graph` / `mark_list` / `mark_fetch` / `mark_backlinks`) with bare paths.
- **a project slug** (e.g. `demarkus`) → scope to `/<slug>/`.
- **a `mark://<world>/` URL** → a knowledge-system world. Use the broker MCP tools (`mcp__<slug>__*`) and `mark://` URLs.

Anchor on the scope's hub: `/index.md`, `/<slug>/index.md`, or `mark://<world>/index.md`.

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
- **Untitled docs** — nodes shown as `(no title)` (no H1 / declared title).
- **ADR sequence** — for each `adr/` directory, list it and flag duplicate or gapped `NNNN` prefixes.

## Deep checks (per-doc `mark_fetch` — run on a small scope, or when asked)

These cost one fetch per document, so only run them for a single project / world or when the user asks for a thorough audit. **If you cap or sample, say so explicitly in the report — don't imply full coverage.**

- **Untagged docs** — fetch and check the `tags` metadata is non-empty. An untagged doc is invisible to `mark_lookup`. (This is what the publish gate now prevents going forward; this finds pre-existing ones.)
- **Duplicate content** — compare `content-hash` across fetched docs; identical hashes under different paths are duplicates.
- **Knowledge-system taxonomy** (only for a `mark://<world>/` scope) — fetch the policy from **the same knowledge system the audited world belongs to**, via the same broker MCP server you're auditing through: `mark://root/.well-known/demarkus/policy.md` on that server (its `root` hub — not some other system's). If that policy is `not-found`, skip this check; the system hasn't declared a taxonomy. Otherwise, for each required axis in its `require_tags:`, flag docs missing an `axis:value` tag, and for documented taxonomy values flag tags that use an out-of-vocabulary value. (The gate enforces axis *presence* going forward; this audits the advisory *values* and the back-catalog.)

## Report

Render plainly, grouped by check, most actionable first. For each finding give the path and a one-line suggested fix. Lead with a one-line summary (`N docs, X findings`). Shape:

```
## <scope> — hygiene report  (<N> docs)

### Broken links (<n>)
- /<doc>.md → /<missing-target>.md  (fetch → not-found) — fix the link or restore the target

### Orphans (<n>)
- /plans/<name>.md — exists but no hub links to it; add it to the relevant index.md

### Untagged (<n>)        [deep check — scanned <k>/<N> docs]
- /<slug>/bar.md — no tags; re-publish with metadata.tags

### ADR / index / titles / duplicates …
```

End with a short prioritized "what I'd fix first." If everything is clean, say so plainly.

## Don't

- Don't write to the soul or modify any document. This command is read-only. If the user then asks you to fix something (e.g. "add the orphans to the index"), do that as a separate, explicit step.
- Don't fabricate. If `mark_graph` returns nothing (empty soul) or the scope hub is `not-found`, say so and stop.
- Don't fetch every document by default — the core checks don't need it. Only run the deep checks on a bounded scope, and disclose the bound.
