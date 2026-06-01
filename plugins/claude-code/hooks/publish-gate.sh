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
tags_value="$(sed -n '6p' <<<"${parsed}")"

# Defensive scope: the matcher is broad (mcp__.*__mark_publish), so a stray
# non-publish tool must not be acted on.
case "${tool}" in
  *mark_publish) ;;
  *) exit 0 ;;
esac

# Server scope: gate only the plugin's own soul and knowledge systems joined via
# /knowledge-join — never an unrelated demarkus server the user configured. A
# registered knowledge system carries its own (agent-mirrored) per-slug strictness.
scope="$(publish_gate_scope "${tool}")"
strict_slug=""
case "${scope}" in
  local) ;;                          # plugin soul → global strictness
  ks:*)  strict_slug="${scope#ks:}" ;;
  *)     exit 0 ;;                    # not a server the plugin manages
esac

# Required tag axes (knowledge-system policy, mirrored locally). An axis is
# satisfied by an "axis:value" token in the tags. Only checked when tags are
# present (a tagless publish is already a violation below). Axes are simple
# identifiers; a stray regex metachar in a policy just won't match → reported
# missing, which is the safe direction.
missing_axes=""
if [[ "${tags_ok}" == "1" ]]; then
  required_axes="$(configured_require_tags "${strict_slug}")"
  for axis in ${required_axes}; do
    grep -qE "(^|,)[[:space:]]*${axis}:" <<<"${tags_value}" \
      || missing_axes="${missing_axes:+${missing_axes} }${axis}"
  done
fi

# Properly tagged + valid importance + all required axes present → nothing to do.
[[ "${tags_ok}" == "1" && "${imp_ok}" == "1" && -z "${missing_axes}" ]] && exit 0

target="${url:-the document}"
problems=""
[[ "${tags_ok}" != "1" ]] && problems="no metadata.tags (it will be invisible to mark_lookup)"
if [[ "${imp_ok}" != "1" ]]; then
  [[ -n "${problems}" ]] && problems="${problems}; "
  problems="${problems}metadata.importance outside [0,1]"
fi
if [[ -n "${missing_axes}" ]]; then
  [[ -n "${problems}" ]] && problems="${problems}; "
  problems="${problems}missing required tag axes for this knowledge system: ${missing_axes} (tag as \"axis:value\", e.g. ${missing_axes%% *}:<value>)"
fi
reason="demarkus publish to ${target} has ${problems}. Re-issue mark_publish with a metadata object: tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. Tags are what make this document findable via mark_lookup."

strictness="$(configured_strictness "${strict_slug}")"

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
