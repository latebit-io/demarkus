#!/usr/bin/env bash
# Promote-nudge: a doc-type promotion trigger for the demarkus-memory plugin.
#
# Wired PostToolUse on the plugin's own mark_publish. When a HIGH-SIGNAL soul
# document is published — today that means an ADR (the plan's named doc-type
# trigger) — and a knowledge endpoint exists to promote to, inject a discreet
# reminder that the decision can be /promote'd up to the shared catalog.
#
# This is the memory-side trigger half of the promote bridge: triggers observe
# the staging tier (the soul), so they live here, not in demarkus-knowledge.
# Strictly a nudge — additionalContext, never decision:block. It fires only when
# all of these hold, to keep false positives near zero (recall-nudge philosophy):
#   - PostToolUse on a mark_publish to THIS plugin's soul (not a knowledge system,
#     not an unrelated server);
#   - a knowledge endpoint is joined (otherwise promote is dormant — nothing to
#     nudge toward);
#   - the path is a high-signal type (an ADR);
#   - the document is not ALREADY promoted (its body carries no `promoted:`
#     marker) — so a back-stamp re-publish does not re-nudge.
#
# Pure awk/grep, zero deps. FAILS OPEN (no nudge) on anything unexpected: -e is
# deliberately NOT set so a stray non-zero from a probe can't misfire the exit.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

# No soul configured → nothing is being staged here.
[[ -f "${PLUGIN_CONFIG}" ]] || exit 0

# No promote destination of any kind (brokered system or plain remote target) →
# promote is dormant, so do not nudge toward it.
promote_destination_present || exit 0

input="$(cat)"

# Reuse the gate's JSON scanner (event, tool, tags_ok, imp_ok, url, tags).
parsed="$(printf '%s' "${input}" | publish_metadata_check)" || exit 0
event="$(sed -n '1p' <<<"${parsed}")"
tool="$(sed -n '2p' <<<"${parsed}")"
url="$(sed -n '5p' <<<"${parsed}")"

# Only after a successful publish, and only for a publish tool.
[[ "${event}" == "PostToolUse" ]] || exit 0
case "${tool}" in
  *mark_publish) ;;
  *) exit 0 ;;
esac

# Only the plugin's own soul — never a knowledge system or unrelated server.
case "$(publish_gate_scope "${tool}")" in
  local) ;;
  *)     exit 0 ;;
esac

# High-signal doc-type: an ADR. Path carries an `/adr/` segment and a .md leaf.
# Tight on purpose — the broader signal-based triggers (recall-count) wait on
# telemetry that does not exist yet.
case "${url}" in
  */adr/*.md) ;;
  *) exit 0 ;;
esac

# Already promoted? A back-stamped doc carries a `promoted: mark://…` marker in
# its body (which is in this very publish payload). Skip so a re-publish of an
# already-promoted ADR does not re-nudge. Grep the raw payload — the marker is
# distinctive enough that a coincidental match is not a concern.
printf '%s' "${input}" | grep -q 'promoted: mark://' && exit 0

nudge="New ADR published to the soul (${url}). ADRs are high-signal, shared-team knowledge: if this decision is durable and useful beyond you, /promote ${url} to lift it into the knowledge system (curate → gate → publish). If it is personal or provisional, leave it here — most soul content correctly stays put."

printf '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":%s}}\n' \
  "$(printf '%s' "${nudge}" | json_escape)"
