#!/usr/bin/env bash
# Destination gate for the demarkus-memory plugin.
#
# Wired in plugin.json as a PreToolUse hook on mark_publish AND mark_append. When
# a project is bound to a specific soul (recorded by /soul-join in
# ~/.demarkus/project-souls), a write aimed at a DIFFERENT soul is a misroute —
# the exact "which soul did I just write to?" confusion this whole feature exists
# to kill. This gate makes the misroute loud at write time instead of letting it
# land silently on the wrong store.
#
#   STRICTNESS=block (default) → deny the misrouted write (agent self-corrects to
#                                the bound soul — no human interruption).
#   STRICTNESS=ask             → escalate to the user.
#   STRICTNESS=warn            → allow, but inject a warning into the model.
#
# Scope is narrow and fails OPEN: it acts ONLY when (a) the tool targets a soul
# this plugin routes (local or a joined remote — never a knowledge system or an
# unrelated server) AND (b) this project has an explicit binding. No binding →
# defer, so the overwhelming majority of projects see zero behavior change.
#
# Zero runtime deps: payload parsed with pure awk (publish_metadata_check). -e is
# deliberately NOT set — a stray non-zero from a probe must not abort into a
# misread hook exit. On any parse hiccup the gate defers.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

input="$(cat)"

parsed="$(printf '%s' "${input}" | publish_metadata_check)" || {
  warn "dest gate: payload parse failed; deferring"
  exit 0
}

event="$(sed -n '1p' <<<"${parsed}")"
tool="$(sed -n '2p' <<<"${parsed}")"
url="$(sed -n '5p' <<<"${parsed}")"

# PreToolUse only — the whole point is to stop the write before it lands.
[[ "${event}" == "PreToolUse" ]] || exit 0

# Defensive scope: the matcher is broad, so act only on the write verbs.
case "${tool}" in
  *mark_publish | *mark_append) ;;
  *) exit 0 ;;
esac

# Only soul writes this plugin routes are in scope (empty → knowledge system or
# an unrelated server: not ours to gate).
target_id="$(soul_target_id "${tool}")"
[[ -n "${target_id}" ]] || exit 0

# No project binding → no destination to enforce against. Fail open.
project_dir="${CLAUDE_PROJECT_DIR:-${PWD}}"
bound="$(project_soul_binding "${project_dir}")"
[[ -n "${bound}" ]] || exit 0

# Correct destination → nothing to do.
[[ "${target_id}" == "${bound}" ]] && exit 0

target="${url:-this document}"
reason="demarkus write to ${target} is going to soul '${target_id}', but this project is bound to soul '${bound}'. Re-issue the write against the bound soul's tools (mcp__${bound}__mark_publish / mcp__${bound}__mark_append). To change which soul this project uses, run /soul-join in this repo; to relax this check, set DEMARKUS_MEMORY_DEST_STRICTNESS=warn (or ask)."

emit_decision() {
  local decision="$1" why="$2"
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"%s","permissionDecisionReason":%s}}\n' \
    "${decision}" "$(printf '%s' "${why}" | json_escape)"
}

emit_context() {
  local ctx="$1"
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":%s}}\n' \
    "$(printf '%s' "${ctx}" | json_escape)"
}

case "$(configured_dest_strictness)" in
  block) emit_decision "deny" "${reason}" ;;
  ask)   emit_decision "ask"  "${reason}" ;;
  warn)  emit_context  "⚠️ ${reason}" ;;
  *)     exit 0 ;;
esac
