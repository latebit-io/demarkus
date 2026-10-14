#!/usr/bin/env bash
# Activity sentinels for the stop nudge: afterFileEdit marks "changed" and
# clears "journal-write" (the entry is stale), afterMCPExecution on a journal
# write marks "journal-write", sessionEnd clears
# the conversation's directory. Cursor transcripts are off by default, so this
# replaces transcript parsing. Never blocks; emits nothing.
set -uo pipefail
payload="$(cat)"
field() { printf '%s' "${payload}" | sed -n "s/.*\"$1\" *: *\"\([^\"]*\)\".*/\1/p" | head -1; }
cid="$(field conversation_id)"
[[ -n "${cid}" ]] || exit 0
dir="${TMPDIR:-/tmp}/demarkus-memory-cursor-${cid//[^A-Za-z0-9_-]/_}"
# Journal write: route today, or a url under journal/ (config.IsJournalWrite).
# Tolerates tool_input arriving as an escaped JSON string.
journal_write() {
  printf '%s' "${payload}" | grep -Eq 'route\\*"[[:space:]]*:[[:space:]]*\\*"today|url\\*"[[:space:]]*:[[:space:]]*\\*"[^"]*/journal/'
}
mark() {
  mkdir -p "${dir}" && : > "${dir}/$1" || echo "[demarkus] could not record the $1 sentinel in ${dir}; the stop nudge may miss it" >&2
}
case "$(field hook_event_name)" in
  afterFileEdit)
    mark changed
    rm -f "${dir}/journal-write" || echo "[demarkus] could not clear the journal-write sentinel in ${dir}" >&2 ;;
  afterMCPExecution)
    case "$(field tool_name)" in
      *mark_publish|*mark_append) if journal_write; then mark journal-write; fi ;;
    esac ;;
  sessionEnd)
    rm -rf "${dir}" || echo "[demarkus] could not clear ${dir}; stale sentinels may linger" >&2 ;;
esac
exit 0
