# Knowledge System Policy

policy_version: 1
owner: <team / contact, e.g. platform-eng (#knowledge-base)>

strictness: warn
require_tags: category

The write policy for this demarkus knowledge system. Joined agents (via the demarkus-knowledge Claude Code plugin's `/knowledge-join`) fetch this from `mark://root/.well-known/demarkus/policy.md` and follow it. Edit it here on `root` to change the policy for everyone; it is versioned like any demarkus document and propagates to each member on their next session.

The lines above are the **enforced core**: the agent mirrors them into the plugin's local gate (`strictness:` → severity, `require_tags:` → mandatory tag axes, and the optional `require_fields:` → mandatory OKF metadata fields such as `type`). `require_fields:` is omitted here so `type` stays advisory until the corpus is typed; add it (e.g. `require_fields: type`) to make it mandatory. Everything below is **convention** the agent follows; the `/knowledge-doctor` hygiene auditor can check it after the fact.

## What to publish here

This is a shared catalog, not a scratch pad. Publish when the content is useful to another team and ready to rely on.

- Keep drafts and personal working notes in your own soul; promote to a world when ready.
- Never publish secrets, credentials, tokens, or PII.
- Prefer one well-tagged document over scattered fragments.

## Required metadata on every publish

Enforced by the plugin's publish gate, at the severity set by `strictness:` above:

- `tags`: comma-separated subjects; never empty (an untagged doc is invisible to `mark_lookup`).
- `importance`: float 0–1, chosen deliberately; reserve ≥0.8 for hubs, architecture, and key decisions.

## Tag taxonomy

Tag as `axis:value` so the catalog stays coherent. The axes named in `require_tags:` are mandatory; the gate checks each is present.

- `category:`: the **domain / area** a document belongs to, e.g. `category:project`, `category:ops`, `category:governance`, `category:platform`. This is what the doc is *about*, not what *kind* it is (kind is the OKF `type` field; see below). Tags are query-rankable in `mark_lookup`, so `category:` is the discoverability axis; keep the set small and stable.
- As the catalog grows and more teams write, add axes (`team:`, `status:`) and list them in `require_tags:` above to enforce them too.

## Document type (OKF `type` field)

Distinct from `category:`. `type` is the document's **kind**, carried in the OKF-native `type` metadata field, **not** a tag. Set it on `mark_publish` as a `type` key in the `metadata` object (`metadata: {"type": "Reference"}`); the server stores it bare and, when omitted, defaults `type: Document` (except `index.md`/`log.md`, which are exempt and stay untyped). Recommended kinds, keep small and stable:

> `Reference`, `Decision` (ADR), `Architecture`, `Plan`, `Journal`, `Guide`, `Note`, `Test`: with `Document` as the generic fallback.

Why a field and not a `type:` tag: `type` is the OKF-standard kind axis, so it exports cleanly to OKF bundles and is filterable (`mark_lookup` `filter: type=Reference`). Rule of thumb: **`category:` = what it's about, `type` = what kind it is.** `type` is advisory until listed in `require_fields:` above.

## Strictness levels

- `warn`: a tagless / out-of-range / missing-axis publish is allowed but the agent is nudged to fix it.
- `block`: denied until fixed. Recommended once the catalog has several active writers.
- `ask`: escalated to the user.

## Structure

Each world follows `template.md` alongside this file: the canonical per-project layout (index hub, `architecture.md`, `patterns.md`, `guidelines.md`, `debugging.md`, `roadmap.md`, `debt.md`, `thoughts.md`, `adr/`, `plans/`, `journal/`).
