#!/usr/bin/env bash
# Shell-level tests for hooks/session-journal-nudge.sh (the Stop hook).
#
# The hook is a stdin→stdout filter: it reads a Stop payload, inspects the
# referenced transcript, and either emits a {"decision":"block",...} nudge or
# nothing (allow stop). We build synthetic transcript JSONL fixtures + Stop
# payloads and assert on stdout. Each case runs with its own TMPDIR so the
# once-per-session sentinel files never collide. Pure awk/bash — no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
HOOK="${PLUGIN_DIR}/hooks/session-journal-nudge.sh"

[[ -x "${HOOK}" ]] || { echo "session-journal-nudge.sh not executable at ${HOOK}"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

# Transcript fixture fragments (compact JSONL, as Claude Code serializes them).
EDIT_LINE='{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/x"}}]}}'
WRITE_LINE='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{}}]}}'
READ_LINE='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}'
PUBLISH_NAME_LINE='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__demarkus-memory__mark_publish","input":{}}]}}'
APPEND_NAME_LINE='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__demarkus-soul__mark_append","input":{}}]}}'
PUBLISH_ATTR_LINE='{"type":"user","attributionMcpServer":"demarkus-memory","attributionMcpTool":"mark_publish","toolUseResult":{}}'
PROSE_DECOY_LINE='{"type":"assistant","message":{"content":[{"type":"text","text":"I should call mark_publish and use name Edit somewhere"}]}}'

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then echo "PASS  ${name}"; PASS=$((PASS + 1))
  else echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'; FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}"); fi
}

# run_nudge TMP SID ACTIVE TRANSCRIPT_FILE — invoke the hook with an isolated
# TMPDIR (for the sentinel), a session id, the stop_hook_active flag, and a
# transcript path. Echoes the hook's stdout.
run_nudge() {
  local tmp="$1" sid="$2" active="$3" tfile="$4"
  local payload
  payload="$(printf '{"session_id":"%s","transcript_path":"%s","cwd":"/x","hook_event_name":"Stop","stop_hook_active":%s}' \
    "${sid}" "${tfile}" "${active}")"
  printf '%s' "${payload}" | TMPDIR="${tmp}" bash "${HOOK}"
}

# mk_transcript TMP LINE... — write the given JSONL lines to a transcript file
# in TMP and echo its path.
mk_transcript() {
  local tmp="$1"; shift
  local f="${tmp}/transcript.jsonl"
  : > "${f}"
  local line
  for line in "$@"; do printf '%s\n' "${line}" >> "${f}"; done
  printf '%s' "${f}"
}

# ----- tests ------------------------------------------------------------

test_files_changed_no_soul_write_blocks() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}" "${READ_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S1" "false" "${t}")"
  rm -rf "${tmp}"
  grep -q '"decision":"block"' <<<"${out}" \
    || { echo "expected block nudge; got: ${out}"; return 1; }
  grep -q 'soul-journal' <<<"${out}" \
    || { echo "reason should point at /soul-journal: ${out}"; return 1; }
}

test_soul_write_via_attribution_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}" "${PUBLISH_ATTR_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S2" "false" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "soul write (attribution) present → no nudge; got: ${out}"; return 1; }
}

test_soul_write_via_toolname_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}" "${PUBLISH_NAME_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S3" "false" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "soul write (tool name) present → no nudge; got: ${out}"; return 1; }
}

test_soul_append_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${WRITE_LINE}" "${APPEND_NAME_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S4" "false" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "mark_append counts as soul write → no nudge; got: ${out}"; return 1; }
}

test_no_file_changes_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${READ_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S5" "false" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "no file mutations → no nudge; got: ${out}"; return 1; }
}

test_prose_decoy_does_not_count_as_soul_write() {
  # A text block that says "mark_publish" must NOT be read as a real write, so a
  # file-changing session with only a prose mention still nudges.
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}" "${PROSE_DECOY_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S6" "false" "${t}")"
  rm -rf "${tmp}"
  grep -q '"decision":"block"' <<<"${out}" \
    || { echo "prose mention of mark_publish must not suppress the nudge: ${out}"; return 1; }
}

test_stop_hook_active_suppresses() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}")"
  local out; out="$(run_nudge "${tmp}" "S7" "true" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "stop_hook_active=true must suppress the nudge; got: ${out}"; return 1; }
}

test_quoted_stop_hook_active_suppresses() {
  # Defensive: a nonconforming payload with stop_hook_active as a quoted string
  # "true" must still be honored as a loop guard.
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}")"
  local payload out
  payload="$(printf '{"session_id":"S7q","transcript_path":"%s","stop_hook_active":"true"}' "${t}")"
  out="$(printf '%s' "${payload}" | TMPDIR="${tmp}" bash "${HOOK}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "quoted stop_hook_active=\"true\" must suppress; got: ${out}"; return 1; }
}

test_once_per_session() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}")"
  local first second
  first="$(run_nudge "${tmp}" "S8" "false" "${t}")"
  second="$(run_nudge "${tmp}" "S8" "false" "${t}")"   # same TMPDIR + sid → sentinel set
  rm -rf "${tmp}"
  grep -q '"decision":"block"' <<<"${first}" \
    || { echo "first call should nudge: ${first}"; return 1; }
  [[ -z "${second}" ]] || { echo "second call same session must be silent (sentinel); got: ${second}"; return 1; }
}

test_missing_session_id_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local t; t="$(mk_transcript "${tmp}" "${EDIT_LINE}")"
  local out; out="$(run_nudge "${tmp}" "" "false" "${t}")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "missing session_id → no nudge; got: ${out}"; return 1; }
}

test_unreadable_transcript_no_block() {
  local tmp; tmp="$(mktemp -d)"
  local out; out="$(run_nudge "${tmp}" "S9" "false" "${tmp}/does-not-exist.jsonl")"
  rm -rf "${tmp}"
  [[ -z "${out}" ]] || { echo "missing transcript → no nudge; got: ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== session-journal-nudge shell tests ==="
for t in ${TESTS}; do
  run_test "${t#test_}"
done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
