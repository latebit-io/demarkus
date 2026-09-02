#!/usr/bin/env bash
# Stop adapter for the session-end journal nudge. The Stop mechanics are
# Claude-specific and stay here: parse the payload, guard against re-blocking,
# and derive the two signals (did the session change files? did it write to a
# memory?) from the transcript. The DECISION + text comes from the shared
# demarkus-plugin binary. Self-contained (no lib.sh) — the small JSON field
# extractor is inlined so the bash infra can be fully retired.
#
# Stop hooks surface a message only by blocking (decision:block forces one more
# turn). Two loop guards prevent wedging: stop_hook_active, and a per-session
# sentinel so it fires at most once. Fails open on anything unexpected.

set -uo pipefail

BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0

# stop_hook_fields — read a flat Stop payload on stdin, emit 3 lines:
# session_id, transcript_path, stop_hook_active ("true"/"false"). Pure awk,
# string-boundary aware (a quote inside a value can't end the string early).
stop_hook_fields() {
  awk '
    { data = data $0 "\n" }
    END {
      n = split(data, ch, ""); depth = 0; inStr = 0; esc = 0; buf = ""; curkey = ""
      sid = ""; tpath = ""; active = "false"
      for (i = 1; i <= n; i++) {
        c = ch[i]
        if (inStr) {
          if (esc) { buf = buf c; esc = 0; continue }
          if (c == "\\") { buf = buf c; esc = 1; continue }
          if (c == "\"") { inStr = 0; onstr(buf); buf = ""; continue }
          buf = buf c; continue
        }
        if (c == "\"") { inStr = 1; buf = ""; continue }
        if (c == " " || c == "\t" || c == "\n" || c == "\r") continue
        if (c == "{") { depth++; expect[depth] = "key"; continue }
        if (c == "[") { depth++; expect[depth] = "val"; continue }
        if (c == "}" || c == "]") { depth--; continue }
        if (c == ":") { expect[depth] = "val"; continue }
        if (c == ",") { expect[depth] = "key"; continue }
        sv = c
        while (i + 1 <= n) {
          d = ch[i+1]
          if (d == "," || d == "}" || d == "]" || d == " " || d == "\t" || d == "\n" || d == "\r") break
          sv = sv d; i++
        }
        if (depth == 1 && curkey == "stop_hook_active") active = sv
        expect[depth] = "key"
      }
      print sid; print tpath; print active
    }
    function onstr(s) {
      if (depth == 1 && expect[depth] == "key") { curkey = s; return }
      if (depth == 1) {
        if (curkey == "session_id") sid = s
        else if (curkey == "transcript_path") tpath = s
        else if (curkey == "stop_hook_active") active = s
      }
    }
  '
}

input="$(cat)"
parsed="$(printf '%s' "${input}" | stop_hook_fields)" || exit 0
sid="$(sed -n '1p' <<<"${parsed}")"
tpath="$(sed -n '2p' <<<"${parsed}")"
active="$(sed -n '3p' <<<"${parsed}")"

[[ "${active}" == "true" ]] && exit 0
[[ -n "${sid}" ]] || exit 0
sentinel="${TMPDIR:-/tmp}/demarkus-memory-nudge-${sid//[^A-Za-z0-9_-]/_}"
[[ -e "${sentinel}" ]] && exit 0
[[ -n "${tpath}" && -r "${tpath}" ]] || exit 0

soul_write="false"
if grep -qE '"attributionMcpTool":"mark_(publish|append)"|"name":"mcp__[^"]*mark_(publish|append)"' "${tpath}"; then
  soul_write="true"
fi
changed_files="false"
if grep -qE '"name":"(Edit|Write|NotebookEdit)"' "${tpath}"; then
  changed_files="true"
fi

out="$("${BIN}" nudge --event session-end --changed-files="${changed_files}" --soul-write="${soul_write}" --format claude < /dev/null || true)"
if [[ -n "${out}" ]]; then
  : > "${sentinel}" 2>/dev/null || true
  printf '%s\n' "${out}"
fi
exit 0
