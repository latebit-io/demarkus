#!/usr/bin/env bash
# Activity sentinels for the stop nudge: afterFileEdit marks "changed" and
# clears "journal-write" (the entry is stale), afterMCPExecution on a journal
# write marks "journal-write", sessionEnd clears the conversation's directory.
# Cursor transcripts are off by default, so this replaces transcript parsing.
# Never blocks; emits nothing.
set -uo pipefail
payload="$(cat)"
field() { printf '%s' "${payload}" | sed -n "s/.*\"$1\" *: *\"\([^\"]*\)\".*/\1/p" | head -1; }
cid="$(field conversation_id)"
[[ -n "${cid}" ]] || exit 0
dir="${TMPDIR:-/tmp}/demarkus-memory-cursor-${cid//[^A-Za-z0-9_-]/_}"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
# The binary owns the journal-write rule and the JSON parsing: exit 0 yes, 1 no.
journal_write() {
  [[ -x "${BIN}" ]] || { echo "[demarkus] ${BIN} missing; journal writes go unrecorded" >&2; return 1; }
  local rc=0
  printf '%s' "${payload}" | "${BIN}" journal-write || rc=$?
  [[ "${rc}" -le 1 ]] || echo "[demarkus] journal-write check failed (exit ${rc})" >&2
  return "${rc}"
}
mark() {
  mkdir -p "${dir}" && : > "${dir}/$1" || echo "[demarkus] could not record the $1 sentinel in ${dir}; the stop nudge may miss it" >&2
}
case "$(field hook_event_name)" in
  afterFileEdit)
    mark changed
    # Builtin test first: this hook runs on every edit and the sentinel is rarely there.
    if [[ -e "${dir}/journal-write" ]]; then
      rm -f "${dir}/journal-write" || echo "[demarkus] could not clear the journal-write sentinel in ${dir}" >&2
    fi ;;
  afterMCPExecution)
    case "$(field tool_name)" in
      *mark_publish|*mark_append) if journal_write; then mark journal-write; fi ;;
    esac ;;
  sessionEnd)
    rm -rf "${dir}" || echo "[demarkus] could not clear ${dir}; stale sentinels may linger" >&2 ;;
esac
exit 0
