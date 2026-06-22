#!/usr/bin/env bash
# Shell-level tests for hooks/dest-gate.sh — the project→soul destination gate.
#
# Each test runs in its own throwaway HOME with a registered remote soul "soul"
# and a binding /repo → soul, then drives the gate with synthetic PreToolUse
# payloads and asserts on the decision JSON. Strictness and project dir are
# forced per-case via env. Pure bash/awk — nothing to install.

# Lib path is computed at runtime (${LIB}); the sourced file is checked on its own.
# shellcheck disable=SC1090
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
LIB="${PLUGIN_DIR}/scripts/lib.sh"
GATE="${PLUGIN_DIR}/hooks/dest-gate.sh"

[[ -f "${LIB}" ]]  || { echo "lib.sh not found at ${LIB}"; exit 2; }
[[ -x "${GATE}" ]] || { echo "dest-gate.sh not executable at ${GATE}"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then
    echo "PASS  ${name}"; PASS=$((PASS + 1))
  else
    echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'
    FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
  fi
}

# setup_env — fresh HOME with remote soul "soul" registered + /repo bound to it.
setup_env() {
  local t; t="$(mktemp -d)"; export HOME="${t}"
  . "${LIB}"
  register_remote_soul "soul" "mark://soul.demarkus.io" 1 "-"
  bind_project_soul "/repo" "soul"
}

# run_gate STRICTNESS PROJECT_DIR EVENT TOOL — drive the gate, echo its stdout.
run_gate() {
  local strictness="$1" proj="$2" event="$3" tool="$4"
  local payload
  payload="$(printf '{"hook_event_name":"%s","tool_name":"%s","tool_input":{"url":"/proj/x.md"}}' "${event}" "${tool}")"
  printf '%s' "${payload}" \
    | CLAUDE_PROJECT_DIR="${proj}" DEMARKUS_MEMORY_DEST_STRICTNESS="${strictness}" bash "${GATE}"
}

LOCAL="mcp__demarkus-memory__mark_publish"
LOCAL_PREFIXED="mcp__plugin_demarkus-memory_demarkus-memory__mark_publish"
BOUND="mcp__soul__mark_publish"
BOUND_APPEND="mcp__soul__mark_append"
LOCAL_APPEND="mcp__demarkus-memory__mark_append"
KNOWLEDGE="mcp__knowledge__mark_publish"

# ----- tests ------------------------------------------------------------------

test_correct_destination_defers() {
  setup_env
  local out; out="$(run_gate block /repo PreToolUse "${BOUND}")"
  [[ -z "${out}" ]] || { echo "write to bound soul should defer; got: ${out}"; return 1; }
}

test_correct_destination_append_defers() {
  setup_env
  local out; out="$(run_gate block /repo PreToolUse "${BOUND_APPEND}")"
  [[ -z "${out}" ]] || { echo "append to bound soul should defer; got: ${out}"; return 1; }
}

test_misroute_to_local_block_denies() {
  setup_env
  local out; out="$(run_gate block /repo PreToolUse "${LOCAL}")"
  grep -q '"permissionDecision":"deny"' <<<"${out}" || { echo "misroute should deny: ${out}"; return 1; }
  grep -q "bound to soul 'soul'" <<<"${out}"        || { echo "reason should name the bound soul: ${out}"; return 1; }
}

test_misroute_prefixed_local_block_denies() {
  setup_env
  local out; out="$(run_gate block /repo PreToolUse "${LOCAL_PREFIXED}")"
  grep -q '"permissionDecision":"deny"' <<<"${out}" || { echo "plugin-prefixed local misroute should deny: ${out}"; return 1; }
}

test_misroute_append_block_denies() {
  setup_env
  local out; out="$(run_gate block /repo PreToolUse "${LOCAL_APPEND}")"
  grep -q '"permissionDecision":"deny"' <<<"${out}" || { echo "append misroute should deny: ${out}"; return 1; }
}

test_misroute_ask_escalates() {
  setup_env
  local out; out="$(run_gate ask /repo PreToolUse "${LOCAL}")"
  grep -q '"permissionDecision":"ask"' <<<"${out}" || { echo "ask strictness should escalate: ${out}"; return 1; }
}

test_misroute_warn_injects_context_no_decision() {
  setup_env
  local out; out="$(run_gate warn /repo PreToolUse "${LOCAL}")"
  grep -q '"additionalContext"' <<<"${out}"  || { echo "warn should inject context: ${out}"; return 1; }
  grep -q 'permissionDecision' <<<"${out}"    && { echo "warn must not carry a decision: ${out}"; return 1; }
  return 0
}

test_no_binding_defers() {
  setup_env
  # An unbound project dir → no enforcement, even on a "wrong" soul.
  local out; out="$(run_gate block /elsewhere PreToolUse "${LOCAL}")"
  [[ -z "${out}" ]] || { echo "unbound project should defer; got: ${out}"; return 1; }
}

test_knowledge_target_defers() {
  setup_env
  # A knowledge system is not a soul this plugin routes → never gated here.
  local out; out="$(run_gate block /repo PreToolUse "${KNOWLEDGE}")"
  [[ -z "${out}" ]] || { echo "knowledge target should defer; got: ${out}"; return 1; }
}

test_post_event_defers() {
  setup_env
  # The gate is PreToolUse-only; a PostToolUse payload must defer.
  local out; out="$(run_gate block /repo PostToolUse "${LOCAL}")"
  [[ -z "${out}" ]] || { echo "PostToolUse should defer; got: ${out}"; return 1; }
}

test_unknown_strictness_falls_back_to_block() {
  setup_env
  local out; out="$(run_gate bogus /repo PreToolUse "${LOCAL}")"
  grep -q '"permissionDecision":"deny"' <<<"${out}" || { echo "unknown strictness should fall back to block (deny): ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== dest-gate shell tests ==="
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
