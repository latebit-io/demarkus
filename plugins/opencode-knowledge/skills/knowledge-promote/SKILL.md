---
name: knowledge-promote
description: Curate a staged document into a shared knowledge system through triage, distillation, deduplication, routing, approval, and provenance-aware publishing.
---

# Knowledge Promote

This is the knowledge side of the promotion bridge. Most staged content should stay personal. Publish only durable material useful to others and ready to become authoritative.

## Inputs

- Source body and origin version for provenance.
- Destination kind:
  - Brokered system: MCP slug, with worlds discovered through `mark_worlds`.
  - Plain remote endpoint: MCP slug and predeclared write path.

## Cascade

1. Triage. Confirm the material is durable, broadly useful, and not already covered. Otherwise stop with "stays personal" and the reason.
2. Distill. Remove personal framing and machine-local detail while preserving substance. Remove every secret, credential, token, and piece of PII.
3. Deduplicate. Use `mark_lookup`, then fetch close matches. Prefer updating an authoritative document over creating a near-duplicate. Surface conflicts rather than overwriting them.
4. Retag. Fetch destination policy. Satisfy each required `axis:value` tag and required metadata field. Choose deliberate importance; do not copy loose soul tags unchanged.
5. Route:
   - Brokered: call `mark_worlds`, consider only writable worlds, then fetch each candidate's `.well-known/demarkus/world.md`. Route by domain and team. Ambiguity requires a write-filtered user choice.
   - Plain remote: write below the registered path. Surface `not-permitted`; do not guess another path.
6. Gate. Effective autonomy is the lower of local preference and destination `autonomy_ceiling`. Default to explicit human approval of draft, destination, tags, and conflict findings.
7. Publish. Fetch before updates and use current `expected_version`; use 0 for creation. Send metadata out of band and include a provenance link in the body.
8. Return destination URL and version. The memory-side caller applies any source back-stamp; this skill never writes the soul.

## Batch routing

Use a cheap model for triage, extraction, and closed-set tagging. Use a strong model only for survivors needing faithful distillation and conflict judgment. Keep the human gate unless destination policy explicitly permits model verification.

## Don't

- Do not lower the triage bar to increase promotion volume.
- Do not publish secrets, PII, or content to a non-writable world.
- Do not exceed `autonomy_ceiling`.
- Do not silently overwrite conflicts.
- Do not write the source soul.
