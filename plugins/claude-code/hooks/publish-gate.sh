#!/usr/bin/env bash
# Publish tag-gate for the demarkus-memory plugin.
#
# Wired in plugin.json on BOTH PreToolUse and PostToolUse for the plugin's own
# mark_publish tool. One script, two events — it branches on hook_event_name:
#
#   STRICTNESS=warn  (default) → PostToolUse injects an additionalContext
#                                system-reminder after a tagless publish,
#                                nudging a re-publish with tags. Non-blocking.
#   STRICTNESS=block           → PreToolUse denies a tagless publish.
#   STRICTNESS=ask             → PreToolUse escalates to the user.
#
# The whole point: an untagged publish is invisible to mark_lookup forever, and
# the server accepts it silently. This makes that failure loud at write time.
#
# Zero runtime dependencies: the payload is parsed with pure awk (see
# publish_metadata_check in lib.sh) — no jq, no python, nothing that might not
# be installed. FAIL OPEN: if parsing yields nothing usable the gate defers — it
# must never block a legitimate publish over a parse hiccup. -e is deliberately
# NOT set: this is control-flow code and a stray non-zero from a probe must not
# abort into a misread hook exit code.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

input="$(cat)"

# Parse the payload (pure awk; always available). Defensive guard only: if awk
# itself were somehow missing or errored, defer rather than block.
parsed="$(printf '%s' "${input}" | publish_metadata_check)" || {
  warn "publish gate: payload parse failed; deferring"
  exit 0
}

event="$(sed -n '1p' <<<"${parsed}")"
tool="$(sed -n '2p' <<<"${parsed}")"
tags_ok="$(sed -n '3p' <<<"${parsed}")"
imp_ok="$(sed -n '4p' <<<"${parsed}")"
url="$(sed -n '5p' <<<"${parsed}")"

# Defensive scope: the matcher already limits us to mark_publish, but a
# misconfigured matcher must not let the gate act on unrelated tools.
case "${tool}" in
  *mark_publish) ;;
  *) exit 0 ;;
esac

# Properly tagged + valid importance → nothing to enforce.
[[ "${tags_ok}" == "1" && "${imp_ok}" == "1" ]] && exit 0

target="${url:-the document}"
problems=""
[[ "${tags_ok}" != "1" ]] && problems="no metadata.tags (it will be invisible to mark_lookup)"
if [[ "${imp_ok}" != "1" ]]; then
  [[ -n "${problems}" ]] && problems="${problems}; "
  problems="${problems}metadata.importance outside [0,1]"
fi
reason="demarkus publish to ${target} has ${problems}. Re-issue mark_publish with a metadata object: tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. Tags are what make this document findable via mark_lookup."

strictness="$(configured_strictness)"

emit_pre() {
  local decision="$1" why="$2"
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"%s","permissionDecisionReason":%s}}\n' \
    "${decision}" "$(printf '%s' "${why}" | json_escape)"
}

emit_post_context() {
  local ctx="$1"
  printf '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":%s}}\n' \
    "$(printf '%s' "${ctx}" | json_escape)"
}

case "${event}" in
  PreToolUse)
    case "${strictness}" in
      block) emit_pre "deny" "${reason}" ;;
      ask)   emit_pre "ask"  "${reason}" ;;
      *)     exit 0 ;;  # warn is enforced post-call
    esac
    ;;
  PostToolUse)
    case "${strictness}" in
      warn) emit_post_context "⚠️ ${reason}" ;;
      *)    exit 0 ;;  # block/ask already enforced before the call ran
    esac
    ;;
  *)
    exit 0
    ;;
esac
