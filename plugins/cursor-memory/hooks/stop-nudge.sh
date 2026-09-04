#!/usr/bin/env bash
# stop adapter for the session-end journal nudge. Reads the sentinels
# mark-activity.sh left for this conversation and asks the binary whether to
# nudge; a nudge becomes a followup_message (Cursor auto-submits it, capped by
# loop_limit). Fires at most once per conversation. Fails open.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0

payload="$(cat)"
field() { printf '%s' "${payload}" | sed -n "s/.*\"$1\" *: *\"\([^\"]*\)\".*/\1/p" | head -1; }
[[ "$(field status)" == "completed" ]] || exit 0
cid="$(field conversation_id)"
[[ -n "${cid}" ]] || exit 0
dir="${TMPDIR:-/tmp}/demarkus-memory-cursor-${cid//[^A-Za-z0-9_-]/_}"
[[ -d "${dir}" ]] || exit 0
[[ -e "${dir}/nudged" ]] && exit 0

changed=false; [[ -e "${dir}/changed" ]] && changed=true
memory_write=false; [[ -e "${dir}/memory-write" ]] && memory_write=true

status=0
out="$("${BIN}" nudge --event session-end --changed-files="${changed}" --memory-write="${memory_write}" --format cursor < /dev/null)" || status=$?
if [[ "${status}" -ne 0 ]]; then
  echo "[demarkus-memory] session-end nudge failed (exit ${status}); skipping" >&2
  exit 0
fi
if [[ -n "${out}" ]]; then
  : > "${dir}/nudged"
  printf '%s\n' "${out}"
fi
exit 0
