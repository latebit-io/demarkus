---
description: Audit joined knowledge systems for broken links, orphans, policy violations, metadata loss, and catalog hygiene. Read-only.
argument-hint: "[slug | mark://<world>/ | blank = every joined system]"
---

Run a read-only health check over organizational knowledge systems. Never repair findings during this command.

## Scope

- Blank: read every nonblank, non-comment slug from `~/.demarkus/knowledge-systems`, then audit all readable worlds.
- Slug: audit that system's readable worlds.
- `mark://<world>/`: audit one world through the applicable system.

For each system call `mark_worlds`. Record `writable`; a fix is actionable only in a writable world. State that visibility is limited by the caller's read authorization.

## Gather

For every world:

1. Call `mark_graph` on `mark://<world>/index.md` with depth 5. Broker graph state is ephemeral, so this crawl is mandatory before backlink or orphan analysis.
2. Recursively inventory the world with `mark_list mark://<world>/`.
3. For every inventory document absent from the root crawl's successful nodes, including nodes whose root-crawl status was `[error]`, call `mark_graph` on that document with depth 1. This seeds outbound edges from disconnected, failed, and beyond-depth documents, making inbound-edge coverage complete for the inventoried world.
4. Preserve node status, title, outbound edges, and cross-world targets from all crawls. Retry each unsuccessful source once. If any source still fails, mark orphan analysis for that world incomplete and do not report definitive orphans.

## Core Checks

- Broken links: report `[not-found]` crawl nodes directly. For targets absent from crawl nodes, confirm against the appropriate world's inventory before reporting.
- Unroutable references: classify `[error]` links by `mark_worlds` membership. Nonmember hosts are external or cross-system pointers, not missing in-system documents. Retry member-world dispatch errors once.
- Orphans: inventory documents with no inbound edge after the root and supplemental crawls, excluding each root hub. Report the number of successfully crawled inventory documents so the coverage boundary is explicit.
- Stale indexes: broken targets linked from `index.md`.
- Missing hubs: populated subtrees without `index.md`.
- Untitled documents: `[ok]` nodes without a title. Do not double-report missing or error nodes as untitled.
- ADR sequences: duplicate or missing numeric prefixes under each `adr/` directory.

## Deep Checks

Run one fetch per document only for one world or when the user requests thorough coverage. If work is capped or sampled, report exact coverage.

- Untagged: missing tags makes a document invisible to `mark_lookup`.
- Policy compliance: fetch `mark://root/.well-known/demarkus/policy.md` once and check every required tag axis and required metadata field. Include policy strictness in each finding.
- Generic type: flag nonnavigation documents whose type is missing or `Document`. Exempt `index.md` and `log.md`.
- In-body frontmatter: flag bodies whose first nonblank line opens a `---` block containing operational keys such as `version`, `previous-hash`, `archived`, or `meta.*`.
- Style: fetch root `style.md`; count em dashes and detect duplicate heading anchors outside fenced code.
- Duplicate content: compare `content-hash` across fetched documents.
- Prose references: outside code fences, resolve high-confidence ADR references and bare `mark://` URLs against inventories. Parse each citing body's own markdown links; do not rely on crawl edges for orphan documents. Classify existing but unlinked targets separately from dangling targets. Confirm dangling targets with one lookup.

### Legacy Metadata Loss

For documents already flagged as untagged or policy-noncompliant and having multiple versions:

1. Call `mark_versions`. Invalid chains are inconclusive; report `chain-error`.
2. Fetch prior versions newest first until finding one with tags that satisfy current required axes and fields. Cap at 10 version fetches per document and 100 calls total, including `mark_versions`.
3. Report recovered tags, importance, title, type, and source version only as a repair candidate. A later publish may have removed metadata deliberately.

A document untagged since version 1 needs curation, not restoration. Any failed call makes the candidate inconclusive.

If the user later requests repair, do it as a separate operation: force-fetch the current full body, preserve its complete current metadata map, layer confirmed recovered fields, recheck current policy, use current `expected_version`, set `on_conflict: "fail"`, and never add retention without explicit approval.

## Report

Group by system, then world, then check. Lead with `<system>: <world-count> worlds, <doc-count> docs, <finding-count> findings`. Include readable scope, writable status, and deep-check coverage. Order confirmed broken links, policy violations, and unreachable documents before advisory type/style backfills. End with a short prioritized repair list. If clean, say so plainly.

## Don't

- Do not write, republish, archive, or repair anything.
- Do not skip graph crawl before backlink reasoning.
- Do not infer absence from an unauthorized or failed call.
- Do not imply visibility beyond the worlds and documents returned for this identity.
- Do not confuse this with `/soul-doctor`, which audits personal souls.
