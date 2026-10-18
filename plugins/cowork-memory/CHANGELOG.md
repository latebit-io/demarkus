# Changelog

## 0.13.186

Deprecated: `demarkus-memory` now runs in Cowork ([what differs](../claude-code/README.md#claude-cowork)). Uninstall `demarkus-cowork`, then install `demarkus-memory`; this plugin leaves the marketplace in the next release.

## 0.13.166

Step 6 of the memory mechanics move prunes the prose the server now owns. The `remember` skill keeps the recall triggers, the per-project `/<slug>/` layout, the `type` map, the update flow and the record routes; the hub rules, metadata rules, retention, the new-project hub dance and the ADR inventory bounds are gone, since the write gate, the journal route and the server instructions carry them. The session guidance keeps store availability, the slug, the record routes and the commands to reach for and defers the read contract to the server instructions. The project identity partial is gone: `/soul-journal` creates a missing project hub through the route, which now refuses an archived project; `/soul-context` stops on the primer's `status: new` or `status: archived` line, which the primer now answers for a missing or archived hub; `/soul-archive` keeps its scope confirmation; the slug comes from the binding and the route's `path:` line names the subtree it wrote.

`/soul-doctor` and `/promote-scan` call the new `mark_doctor` tool instead of the plugin binary: the server reads the scope once and returns the hygiene report, so a bound broker store can be audited too, and `promotion: true` lists promotion candidates with the already-promoted set excluded. The style rules every publish is held to now run inside `mark_publish` on every surface; the Claude Code hook is unchanged. `/soul-journal` is one `mark_append` call with `route: "today"`: the server picks the day, creates or appends, widens tags and keeps the hubs linked, answering `present` for a repeated entry. `/soul-context` reads one primer, `/<slug>/.context` (hub, newest journal entries, roadmap, plans). Layout names come from the world's `/.well-known/demarkus/template.md` (`journal:`, `plans:`, `roadmap:`, `staging:`, `promote_types:`, `promote_floor:`); the seeded memory template carries the block.

`/soul-curate` is two `mark_curate` calls: the report (findings, the proposed edit or the split into a hub plus topic files with anchors kept, the inbound anchored links to repoint, a proposal hash) and, after the human gate, `apply: true` with that hash; the tool preflights every topic path and writes the hub last at the reported version. `/soul-archive` is one `mark_archive` call: a document, or a directory with `recursive: true`; the tool archives every live document under it and drops the hub bullets that only linked what was archived. The style gate now warns on any document at or over the 8 KB fetch threshold, not only a hub; journals are exempt.

`/promote`, `/soul-refresh` and the knowledge plugin's `knowledge-promote` skill run on the new `mark_promote` tool. The soul server stages the source (`stage`: markers, whether the layout marks it as shared, a packet), writes the back-stamp (`stamp`, marker-only by default, `stub` on request) and refreshes a promoted copy from the live knowledge body (`refresh`: in-sync, stale with the proposed body and a packet, behind). The knowledge server checks the distilled draft (`dry_run`: write grant, policy axes and fields, style, dedup by title and tags with candidate bodies) and publishes it with `promoted-from` provenance (`apply`), each write bound to the packet of the report before it. The memory broker serves the soul phases only; promotion stays client composed.

## 0.13.161

Recall guidance budgets by question shape: an answer in one section omits `budget` and fetches the section; an answer spanning documents or their relations passes `budget: 1500` on the first lookup and cites the appended blocks. The rule lives in the MCP server instructions; the skill and command defer to it.

## 0.13.158

mark_fetch and mark_explore return every metadata key on every call; the verbose argument is gone and no fetch instruction passes it.

## 0.13.153

First release: the Cowork port of `demarkus-memory`. MCP tools through the shared launcher, `/soul`, `/soul-context`, `/soul-journal`, `/soul-archive`, `/soul-curate`, and the `remember` skill; no hooks, no shell commands.
