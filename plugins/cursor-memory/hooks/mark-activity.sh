#!/usr/bin/env bash
# Activity sentinels for the stop nudge: afterFileEdit marks "changed",
# afterMCPExecution on a mark write marks "memory-write", sessionEnd clears
# the conversation's directory. Cursor transcripts are off by default, so this
# replaces transcript parsing. Never blocks; emits nothing.
set -uo pipefail
payload="$(cat)"
field() { printf '%s' "${payload}" | sed -n "s/.*\"$1\" *: *\"\([^\"]*\)\".*/\1/p" | head -1; }
cid="$(field conversation_id)"
[[ -n "${cid}" ]] || exit 0
dir="${TMPDIR:-/tmp}/demarkus-memory-cursor-${cid//[^A-Za-z0-9_-]/_}"
mark() {
  mkdir -p "${dir}" && : > "${dir}/$1" || echo "[demarkus-memory] could not record the $1 sentinel in ${dir}; the stop nudge may miss it" >&2
}
case "$(field hook_event_name)" in
  afterFileEdit) mark changed ;;
  afterMCPExecution)
    case "$(field tool_name)" in
      *mark_publish|*mark_append) mark memory-write ;;
    esac ;;
  sessionEnd)
    rm -rf "${dir}" || echo "[demarkus-memory] could not clear ${dir}; stale sentinels may linger" >&2 ;;
esac
exit 0
