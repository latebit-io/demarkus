# demarkus-knowledge

Connect Claude Code to an **organizational demarkus knowledge system**: a shared, versioned markdown catalog fronted by a demarkus-knowledge-broker over HTTPS.

This is the broker-fronted, multi-writer counterpart to [`demarkus-memory`](../claude-code), which gives you a personal, local soul. The two are independent plugins and compose cleanly: install both and your soul becomes your private scratch space while the knowledge system is the shared, authoritative source of truth. Install just this one to work against your org's catalog without running a local server at all.

## What it does

- **`/knowledge-join <broker-url>`**: validate an org broker (RFC 9728 metadata check), derive a server slug, register it with `claude mcp add --transport http`, and record it so the publish gate covers it. Auth is Claude Code's own MCP OAuth device flow against the broker; no tokens to paste or store locally.
- **`/knowledge`**: list the systems you've joined and show each one's `root` hub index, so the shared catalog is easy to navigate. The counterpart to `/soul`.
- **`/knowledge-doctor`**: read-only hygiene audit of a joined system, per world (orphans, broken links, dangling references, untagged and policy-noncompliant docs, ADR gaps).
- **`knowledge-promote` skill**: the curation cascade that lands a staged document in the catalog: triage (most content correctly stays personal) → distill for a shared audience (stripping personal framing, secrets, PII) → dedup against the catalog → tag to the system taxonomy → select a destination among your **writable** worlds (via `mark_worlds` + each world's `world.md` descriptor) → human gate, capped by the destination's autonomy ceiling → publish with provenance. It is the knowledge-side half of the promote bridge: the demarkus-memory plugin's `/promote` invokes it and applies the back-stamp; this side never writes the soul.
- **Standing guidance (SessionStart)**: when a system is joined, sessions are steered to consult the shared catalog first for shared/org subjects, to navigate via the `root` hub and `mark://<world>/<path>` URLs, and (when a local soul also exists) to keep the soul↔system lanes: draft in the soul, promote to the system when ready. Brokered systems expose `mark_lookup_all`, one catalog lookup across every readable world, and the guidance prefers it over per-world lookup loops.
- **Publish tag-gate (Pre/PostToolUse)**: enforces `tags` (and any required tag axes the system's policy declares) on writes to a joined system, at the severity the system chooses (warn / block / ask), so shared documents stay findable via `mark_lookup`. It gates **only** registered knowledge systems; the local soul and unrelated demarkus servers are left untouched.
- **Recall nudge (UserPromptSubmit)**: on a recall-style question about shared/org knowledge, a discreet reminder to check the catalog first. Silent unless a system is joined.

## Local Helper

This plugin runs no local demarkus server. It bootstraps the shared `demarkus-plugin` helper for gates, guidance, and registry behavior, and writes a small registry and per-system policy mirror under `~/.demarkus/`:

- `~/.demarkus/knowledge-systems`: one joined MCP server slug per line.
- `~/.demarkus/plugin-knowledge.strictness.<slug>`: mirrored per-system gate severity.
- `~/.demarkus/plugin-knowledge.require-tags.<slug>`: mirrored required tag axes.
- `~/.demarkus/plugin-knowledge.require-fields.<slug>`: mirrored required OKF metadata fields (e.g. `type`).

It owns this `plugin-knowledge.*` namespace entirely and never **writes** the demarkus-memory plugin's files, so it stands alone. Its only touch of that state is **read-only**: it checks whether `~/.demarkus/plugin-memory.conf` exists, to tell whether a local soul is configured, so the soul↔system guidance only appears when it's relevant (and is simply omitted when it isn't).

Runtime prompt files under `commands/`, `context/`, and `skills/` are generated from `plugins/prompt-source/`. Edit the canonical templates and run `cd tools && go run ./plugin-prompts write`; generated files are distribution artifacts.

## Admin: publishing conventions

A knowledge system's org-wide policy and per-world template live on its guaranteed `root` hub under `mark://root/.well-known/demarkus/`. See [`examples/knowledge-system/`](examples/knowledge-system/) for the format an admin publishes; joined agents fetch and follow them, mirroring the enforceable knobs (strictness, required tag axes, required OKF fields) into the local gate via `scripts/mirror-policy.sh`.
