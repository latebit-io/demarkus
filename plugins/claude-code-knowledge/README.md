# demarkus-knowledge

Connect Claude Code to an **organizational demarkus knowledge system**: a shared, versioned markdown catalog fronted by a demarkus knowledge gateway over HTTPS.

This is the broker-fronted, multi-writer counterpart to [`demarkus-memory`](../claude-code), which gives you a personal, local memory. The two are independent plugins and compose cleanly: install both and your memory becomes your private scratch space while the knowledge system is the shared, authoritative source of truth. Install just this one to work against your org's catalog without running a local server at all.

## Install

In Claude Code, from a terminal:

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-knowledge@demarkus
/knowledge-join https://<your-broker>
```

Restart Claude Code and complete the OAuth prompt with your work identity. The server is registered at user scope, so every project sees it. Then `/knowledge` shows the root hub and the write policy; ordinary questions about shared subjects consult the catalog on their own. With `demarkus-memory` installed too, `/promote` lifts a ready note from your memory into the system through the `knowledge-promote` skill.

Requirements: macOS or Linux (the helper's installer is bash; Windows is not supported), `curl` and `tar`, and the `claude` CLI on `PATH` for the join.

## What it does

- **`/knowledge-join <broker-url>`**: validate an org broker (RFC 9728 metadata check), derive a server slug, register it with `claude mcp add -s user --transport http`, and record it so the publish gate covers it. Auth is Claude Code's own MCP OAuth flow against the broker; no tokens to paste or store locally.
- **`/knowledge`**: list the systems you've joined and show each one's `root` hub index and write policy, so the shared catalog is easy to navigate. The counterpart to `/soul`.
- **`/knowledge-doctor`**: read-only hygiene audit of a joined system, per world (orphans, broken links, dangling references, untagged and policy-noncompliant docs, secrets and personal data, ADR gaps).
- **`knowledge-promote` skill**: the curation cascade that lands a staged document in the catalog: triage (most content correctly stays personal), distill for a shared audience (stripping personal framing, secrets, PII), route to a **writable** world (via `mark_worlds` and each world's `world.md` descriptor), then `mark_promote` on the broker: `dry_run` checks the draft against the write grant, the world's policy, the style rules and the catalog (dedup by title and tags), a human gate capped by the destination's autonomy ceiling, `apply` publishes with provenance. It is the knowledge-side half of the promote bridge: the demarkus-memory plugin's `/promote` invokes it and applies the back-stamp; this side never writes the memory.
- **Standing guidance (SessionStart)**: when a system is joined, sessions are steered to consult the shared catalog first for shared/org subjects, to navigate via the `root` hub and `mark://<world>/<path>` URLs, and (when a soul also exists) to keep the soul↔system lanes: draft in the soul, promote to the system when ready. Brokered systems expose `mark_lookup_all`, one catalog lookup across every readable world, and the guidance prefers it over per-world lookup loops.
- **Publish tag-gate (Pre/PostToolUse)**: checks `tags` on writes to a joined system before the call leaves the session, so shared documents stay findable via `mark_lookup`. The server enforces the system's write policy (required tag axes, allowed values, required fields, strictness, secrets and personal data) on every write. The gate covers **only** registered knowledge systems; the local soul and unrelated demarkus servers are left untouched.
- **Recall nudge (UserPromptSubmit)**: on a recall-style question about shared/org knowledge, a discreet reminder to check the catalog first. Silent unless a system is joined.

### Claude Cowork

The same plugin loads in Cowork. A knowledge system reaches Cowork as a claude.ai connector (the broker's `/mcp` URL with OAuth) that an admin adds, not through `/knowledge-join`: Cowork's shell and hooks run in a sandbox without `~/.demarkus` or the `claude` CLI. There the session hook injects the standing guidance, gates and nudges stay quiet, `/knowledge` and `/knowledge-doctor` work over the connected servers that offer `mark_worlds`, and `/knowledge-join` says to join from a terminal session on the host. The `knowledge-promote` skill runs over MCP; the memory plugin's `/promote` that invokes it is terminal-only in Cowork for now.

## Local helper

This plugin runs no local demarkus server. The session hook installs the shared `demarkus-plugin` helper (`scripts/bootstrap.sh`) for the gate, guidance and registry where the `claude` CLI is on `PATH`, so a sandboxed session downloads nothing. State under `~/.demarkus/`:

- `~/.demarkus/knowledge-systems`: one joined MCP server slug per line.

It never **writes** the demarkus-memory plugin's files, so it stands alone. Its only touch of that state is **read-only**: it checks whether `~/.demarkus/plugin-memory.conf` exists, to tell whether a local memory is configured, so the memory↔system guidance only appears when it's relevant (and is simply omitted when it isn't).

Runtime prompt files under `commands/`, `context/`, and `skills/` are generated from `plugins/prompt-source/`. Edit the canonical templates and run `cd tools && go run ./plugin-prompts write`; generated files are distribution artifacts.

## Admin: publishing conventions

A knowledge system's org-wide policy and per-world template live on its guaranteed `root` hub under `mark://root/.well-known/demarkus/`. See [`examples/knowledge-system/`](examples/knowledge-system/) for the format an admin publishes; the server enforces the policy on every write and joined agents fetch and follow the template.
