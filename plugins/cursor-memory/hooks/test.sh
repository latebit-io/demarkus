#!/usr/bin/env bash
# Shim contract test: a fake demarkus-plugin records how each hook calls it.
# Run from anywhere: bash plugins/cursor-memory/hooks/test.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
export HOME; HOME="$(mktemp -d)"
export TMPDIR="${HOME}/tmp"; mkdir -p "${TMPDIR}" "${HOME}/.demarkus/bin"
CALLS="${HOME}/calls"
cat > "${HOME}/.demarkus/bin/demarkus-plugin" <<FAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${CALLS}"
in="\$(cat)"
case "\$1" in
  gate) echo '{"permission":"allow","agent_message":"warned"}' ;;
  nudge) case "\$*" in *session-end*) echo '{"followup_message":"journal?"}' ;; *) echo '{"permission":"allow","agent_message":"promote?"}' ;; esac ;;
  guidance) echo '{"additional_context":"guidance"}' ;;
esac
FAKE
chmod +x "${HOME}/.demarkus/bin/demarkus-plugin"
fail() { echo "FAIL: $*" >&2; exit 1; }

cid="11111111-2222-3333-4444-555555555555"
dir="${TMPDIR}/demarkus-memory-cursor-${cid}"

# stop with no activity: silent, no binary call
out="$(printf '{"hook_event_name":"stop","conversation_id":"%s","status":"completed","loop_count":0}' "$cid" | bash "${HERE}/stop-nudge.sh")"
[[ -z "${out}" ]] || fail "stop without activity should be silent: ${out}"
[[ ! -e "${CALLS}" ]] || fail "stop without activity must not call the binary"

# afterFileEdit + afterMCPExecution set sentinels
printf '{"hook_event_name":"afterFileEdit","conversation_id":"%s","file_path":"/x"}' "$cid" | bash "${HERE}/mark-activity.sh"
[[ -e "${dir}/changed" ]] || fail "afterFileEdit should mark changed"
printf '{"hook_event_name":"afterMCPExecution","conversation_id":"%s","tool_name":"mark_fetch","mcp_server_name":"demarkus-memory"}' "$cid" | bash "${HERE}/mark-activity.sh"
[[ ! -e "${dir}/memory-write" ]] || fail "mark_fetch must not mark memory-write"
printf '{"hook_event_name":"afterMCPExecution","conversation_id":"%s","tool_name":"mark_append","mcp_server_name":"demarkus-memory"}' "$cid" | bash "${HERE}/mark-activity.sh"
[[ -e "${dir}/memory-write" ]] || fail "mark_append should mark memory-write"

# stop: forwards both signals, emits followup once
out="$(printf '{"hook_event_name":"stop","conversation_id":"%s","status":"completed","loop_count":0}' "$cid" | bash "${HERE}/stop-nudge.sh")"
[[ "${out}" == '{"followup_message":"journal?"}' ]] || fail "stop should emit the followup: ${out}"
grep -q -- '--changed-files=true --memory-write=true --format cursor' "${CALLS}" || fail "stop passed wrong flags: $(cat "${CALLS}")"
out="$(printf '{"hook_event_name":"stop","conversation_id":"%s","status":"completed","loop_count":0}' "$cid" | bash "${HERE}/stop-nudge.sh")"
[[ -z "${out}" ]] || fail "second stop must be silent: ${out}"
out="$(printf '{"hook_event_name":"stop","conversation_id":"%s","status":"aborted","loop_count":0}' "$cid" | bash "${HERE}/stop-nudge.sh")"
[[ -z "${out}" ]] || fail "aborted stop must be silent"

# sessionEnd clears the sentinels
printf '{"hook_event_name":"sessionEnd","conversation_id":"%s","reason":"closed"}' "$cid" | bash "${HERE}/mark-activity.sh"
[[ ! -d "${dir}" ]] || fail "sessionEnd should clear the sentinel dir"

# gate + promote nudge pipe through with --format cursor
out="$(echo '{"hook_event_name":"beforeMCPExecution","tool_name":"mark_publish","mcp_server_name":"demarkus-memory","tool_input":{}}' | bash "${HERE}/gate.sh")"
[[ "${out}" == '{"permission":"allow","agent_message":"warned"}' ]] || fail "gate output: ${out}"
grep -q '^gate --format cursor$' "${CALLS}" || fail "gate flags wrong"
out="$(echo '{"hook_event_name":"beforeMCPExecution","tool_name":"mark_publish"}' | bash "${HERE}/promote-nudge.sh")"
[[ "${out}" == '{"permission":"allow","agent_message":"promote?"}' ]] || fail "promote output: ${out}"
grep -q '^nudge --event promote --format cursor$' "${CALLS}" || fail "promote flags wrong"

# every shim fails open without the binary
rm "${HOME}/.demarkus/bin/demarkus-plugin"
for s in gate promote-nudge stop-nudge; do
  out="$(echo '{"hook_event_name":"x","conversation_id":"c","status":"completed"}' | bash "${HERE}/${s}.sh")" || fail "${s} exited non-zero without the binary"
  [[ -z "${out}" ]] || fail "${s} should be silent without the binary"
done
echo "cursor-memory hooks: OK"
