#!/usr/bin/env bash
# Shell-level tests for hooks/recall-nudge.sh (the UserPromptSubmit hook).
#
# The hook reads a UserPromptSubmit payload and, when the prompt reads like a
# recall question, emits a non-blocking additionalContext nudge. It is gated on
# the plugin being configured (PLUGIN_CONFIG present), so each case runs with an
# isolated HOME. Pure grep/tr — no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
HOOK="${PLUGIN_DIR}/hooks/recall-nudge.sh"

[[ -x "${HOOK}" ]] || { echo "recall-nudge.sh not executable at ${HOOK}"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then echo "PASS  ${name}"; PASS=$((PASS + 1))
  else echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'; FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}"); fi
}

# mkhome — temp HOME with a plugin config so the hook's gate passes.
mkhome() {
  local h; h="$(mktemp -d)"
  mkdir -p "${h}/.demarkus"
  printf 'SOUL_DIR=%s/.demarkus/soul\nPORT=6310\nMODE=default\n' "${h}" > "${h}/.demarkus/plugin-memory.conf"
  printf '%s' "${h}"
}

# fire HOME PROMPT — run the hook with the given prompt (no quotes/newlines in
# PROMPT for these tests) and echo stdout.
fire() {
  local home="$1" prompt="$2"
  printf '{"session_id":"s","cwd":"/x","hook_event_name":"UserPromptSubmit","prompt":"%s"}' "${prompt}" \
    | HOME="${home}" bash "${HOOK}"
}

# ----- tests ------------------------------------------------------------

test_recall_phrase_nudges() {
  local h; h="$(mkhome)"
  local out; out="$(fire "${h}" "did we decide on the auth model")"
  rm -rf "${h}"
  grep -q '"additionalContext"' <<<"${out}" \
    || { echo "recall phrase should nudge: ${out}"; return 1; }
  grep -q 'mark_lookup' <<<"${out}" \
    || { echo "nudge should mention mark_lookup: ${out}"; return 1; }
  # Must be a non-blocking nudge — no decision/permissionDecision field.
  grep -q 'permissionDecision\|"decision"' <<<"${out}" \
    && { echo "recall nudge must be non-blocking (no decision): ${out}"; return 1; }
  return 0
}

test_various_recall_phrases_nudge() {
  local h; h="$(mkhome)"
  local p out rc=0
  for p in \
    "what did we decide here" \
    "have we shipped the broker yet" \
    "what is our policy on tags" \
    "do we have a note on the gateway" \
    "last time we tried this it broke" \
    "is there an adr for the storage format" \
    "we already discussed the slug heuristic"; do
    out="$(fire "${h}" "${p}")"
    grep -q '"additionalContext"' <<<"${out}" || { echo "no nudge for: '${p}' → ${out}"; rc=1; }
  done
  rm -rf "${h}"
  return "${rc}"
}

test_case_insensitive() {
  local h; h="$(mkhome)"
  local out; out="$(fire "${h}" "What Did We Decide About Caching")"
  rm -rf "${h}"
  grep -q '"additionalContext"' <<<"${out}" \
    || { echo "should match case-insensitively: ${out}"; return 1; }
}

test_plain_prompt_no_nudge() {
  local h; h="$(mkhome)"
  local p out rc=0
  for p in \
    "write a function to sort a list" \
    "refactor the parser to be faster" \
    "explain how quic handshakes work" \
    "add a test for the gate"; do
    out="$(fire "${h}" "${p}")"
    [[ -z "${out}" ]] || { echo "ordinary prompt should not nudge: '${p}' → ${out}"; rc=1; }
  done
  rm -rf "${h}"
  return "${rc}"
}

test_no_config_no_nudge() {
  # No plugin config → memory isn't set up → stay silent even for a recall phrase.
  local h; h="$(mktemp -d)"
  local out; out="$(fire "${h}" "did we decide on the auth model")"
  rm -rf "${h}"
  [[ -z "${out}" ]] || { echo "must not nudge when unconfigured; got: ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== recall-nudge shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
