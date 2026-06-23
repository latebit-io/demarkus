#!/usr/bin/env bash
# Publish tag-gate for the demarkus-knowledge plugin.
#
# Wired in plugin.json on BOTH PreToolUse and PostToolUse for any mark_publish
# tool. It enforces tags ONLY on writes to a knowledge system joined via
# /knowledge-join (a registered slug); every other server — including the local
# soul served by the demarkus-memory plugin, and unrelated demarkus servers — is
# left alone (publish_gate_scope returns empty → defer). With both plugins
# installed the two gates partition cleanly: each is silent unless the publish
# targets its own scope, so at most one emits a decision per publish.
#
# Strictness + required tag axes are per-knowledge-system, mirrored locally from
# the system's published policy (the gate cannot reach the broker):
#   warn  (default) → PostToolUse injects an additionalContext system-reminder.
#   block           → PreToolUse denies.
#   ask             → PreToolUse escalates to the user.
#
# Zero runtime dependencies: pure awk (publish_metadata_check in lib.sh). FAIL
# OPEN: a parse hiccup defers rather than blocks. -e is deliberately NOT set —
# this is control-flow code and a stray non-zero from a probe must not abort
# into a misread hook exit code.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

input="$(cat)"

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
type_value="$(sed -n '7p' <<<"${parsed}")"

# Defensive scope: the matcher is broad (mcp__.*__mark_publish), so a stray
# non-publish tool must not be acted on.
case "${tool}" in
  *mark_publish) ;;
  *) exit 0 ;;
esac

# Server scope: only a registered knowledge system is ours. The local soul and
# any unmanaged server defer.
scope="$(publish_gate_scope "${tool}")"
case "${scope}" in
  ks:*) strict_slug="${scope#ks:}" ;;
  *)    exit 0 ;;
esac

# Required tag axes (knowledge-system policy, mirrored locally). An axis is
# satisfied by an "axis:value" token in the tags. Only checked when tags are
# present (a tagless publish is already a violation below). Axis matching is
# literal (tags_have_axis), so a policy axis containing metacharacters can't
# overmatch and falsely satisfy the check.
missing_axes=""
if [[ "${tags_ok}" == "1" ]]; then
  required_axes="$(configured_require_tags "${strict_slug}")"
  # Split the space-separated axes with globbing off (an axis must never expand
  # against the filesystem); tags_have_axis then matches each literally.
  set -f
  for axis in ${required_axes}; do
    tags_have_axis "${tags_value}" "${axis}" \
      || missing_axes="${missing_axes:+${missing_axes} }${axis}"
  done
  set +f
fi

# Required OKF metadata fields (policy require_fields:, mirrored locally). A
# field is satisfied by a non-empty metadata.<field>. Only `type` is inspected
# today; any other declared field is skipped fail-open — the gate can't see it
# and must never block on what it can't verify.
missing_fields=""
required_fields="$(configured_require_fields "${strict_slug}")"
set -f
for field in ${required_fields}; do
  case "${field}" in
    type)
      # index.md / log.md are server-exempt from the type default
      # (applyOKFTypeDefault) — a hub is intentionally untyped, so never require a
      # type on one. Mirror that exemption here or every hub publish would block.
      case "${url##*/}" in
        index.md|log.md) ;;
        *) [[ -n "${type_value}" ]] || missing_fields="${missing_fields:+${missing_fields} }type" ;;
      esac
      ;;
    *)    warn "publish gate: unsupported require_field '${field}' (only 'type' is checked); skipping" ;;
  esac
done
set +f

# Properly tagged + valid importance + all required axes + all required fields → nothing to do.
[[ "${tags_ok}" == "1" && "${imp_ok}" == "1" && -z "${missing_axes}" && -z "${missing_fields}" ]] && exit 0

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
if [[ -n "${missing_fields}" ]]; then
  [[ -n "${problems}" ]] && problems="${problems}; "
  problems="${problems}missing required OKF metadata fields: ${missing_fields} (set a \"${missing_fields%% *}\" key in the metadata object, e.g. metadata: {\"type\": \"Reference\"})"
fi
reason="demarkus publish to ${target} (knowledge system '${strict_slug}') has ${problems}. Re-issue mark_publish with a metadata object: tags (comma-separated subjects derived from the content) and, if set, importance in [0,1]. Tags are what make this document findable via mark_lookup across the shared catalog."

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
