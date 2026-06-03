# Knowledge System Policy

policy_version: 1
owner: <team / contact, e.g. platform-eng (#knowledge-base)>

strictness: warn
require_tags: category

The write policy for this demarkus knowledge system. Joined agents (via the demarkus-memory Claude Code plugin's `/knowledge-join`) fetch this from `mark://root/.well-known/demarkus/policy.md` and follow it. Edit it here on `root` to change the policy for everyone — it is versioned like any demarkus document and propagates to each member on their next session.

The two lines above are the **enforced core** — the agent mirrors them into the plugin's local gate (`strictness:` → severity, `require_tags:` → mandatory tag axes). Everything below is **convention** the agent follows; a future hygiene auditor can check it after the fact.

## What to publish here

This is a shared catalog, not a scratch pad. Publish when the content is useful to another team and ready to rely on.

- Keep drafts and personal working notes in your own soul; promote to a world when ready.
- Never publish secrets, credentials, tokens, or PII.
- Prefer one well-tagged document over scattered fragments.

## Required metadata on every publish

Enforced by the plugin's publish gate, at the severity set by `strictness:` above:

- `tags` — comma-separated subjects; never empty (an untagged doc is invisible to `mark_lookup`).
- `importance` — float 0–1, chosen deliberately; reserve ≥0.8 for hubs, architecture, and key decisions.

## Tag taxonomy

Tag as `axis:value` so the catalog stays coherent. The axes named in `require_tags:` are mandatory — the gate checks each is present.

- `category:` — the kind/area of the document, e.g. `category:project`, `category:ops`, `category:reference`, `category:test`. Keep the set small and stable.
- As the catalog grows and more teams write, add axes (`team:`, `type:`, `status:`) and list them in `require_tags:` above to enforce them too.

## Strictness levels

- `warn` — a tagless / out-of-range / missing-axis publish is allowed but the agent is nudged to fix it.
- `block` — denied until fixed. Recommended once the catalog has several active writers.
- `ask` — escalated to the user.

## Structure

Each world follows `template.md` alongside this file — the canonical per-project layout (index hub, `architecture.md`, `patterns.md`, `guidelines.md`, `debugging.md`, `roadmap.md`, `debt.md`, `thoughts.md`, `adr/`, `plans/`, `journal/`).
