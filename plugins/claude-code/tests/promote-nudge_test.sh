#!/usr/bin/env bash
# Shell-level tests for promote-nudge.sh — the doc-type promotion trigger.
#
# The hook sources lib.sh, which binds PLUGIN_CONFIG / KNOWLEDGE_REGISTRY to HOME
# at source time, so each case runs the hook in a fresh subprocess with a
# controlled HOME and feeds the JSON payload on stdin. Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
HOOK="${PLUGIN_DIR}/hooks/promote-nudge.sh"

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

# new_home [with_knowledge] — temp HOME with a soul conf; optionally a knowledge
# registry so knowledge_present is true. Echoes the HOME path.
new_home() {
  local home; home="$(mktemp -d)"
  mkdir -p "${home}/.demarkus"
  printf 'SOUL_DIR=%s/soul\nPORT=6310\nMODE=shared\n' "${home}" > "${home}/.demarkus/plugin-memory.conf"
  [[ "${1:-}" == "with_knowledge" ]] && printf 'knowledge\n' > "${home}/.demarkus/knowledge-systems"
  echo "${home}"
}

# payload EVENT TOOL URL BODY — emit a hook JSON payload.
payload() {
  printf '{"hook_event_name":"%s","tool_name":"%s","tool_input":{"url":"%s","body":"%s","metadata":{"tags":"adr"}}}' \
    "$1" "$2" "$3" "$4"
}

SOUL_TOOL="mcp__demarkus-memory__mark_publish"
ADR_URL="/demarkus/adr/0007-promote-triggers.md"

test_nudges_on_adr_publish() {
  local home; home="$(new_home with_knowledge)"
  local out; out="$(payload PostToolUse "${SOUL_TOOL}" "${ADR_URL}" "# 0007. Promote triggers" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  local ok=1
  printf '%s' "${out}" | grep -q '/promote' || { echo "expected a /promote nudge"; ok=0; }
  printf '%s' "${out}" | grep -q "${ADR_URL}" || { echo "nudge should name the ADR path"; ok=0; }
  printf '%s' "${out}" | grep -q 'PostToolUse' || { echo "should be a PostToolUse additionalContext"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_silent_without_knowledge_endpoint() {
  # No knowledge system joined → promote is dormant → no nudge.
  local home; home="$(new_home)"
  local out; out="$(payload PostToolUse "${SOUL_TOOL}" "${ADR_URL}" "# 0007. X" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence without a knowledge endpoint, got: ${out}"; return 1; }
}

test_silent_on_non_adr_path() {
  local home; home="$(new_home with_knowledge)"
  local out; out="$(payload PostToolUse "${SOUL_TOOL}" "/demarkus/patterns.md" "# Patterns" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence on a non-ADR path, got: ${out}"; return 1; }
}

test_silent_when_already_promoted() {
  # Body already carries the back-stamp marker → a re-publish must not re-nudge.
  local home; home="$(new_home with_knowledge)"
  local body="# 0007. X\n\npromoted: mark://world-a/test/x.md@v1"
  local out; out="$(payload PostToolUse "${SOUL_TOOL}" "${ADR_URL}" "${body}" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence for an already-promoted doc, got: ${out}"; return 1; }
}

test_silent_on_pretooluse() {
  # The nudge is a post-publish reminder; PreToolUse is the gate's lane, not ours.
  local home; home="$(new_home with_knowledge)"
  local out; out="$(payload PreToolUse "${SOUL_TOOL}" "${ADR_URL}" "# 0007. X" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence on PreToolUse, got: ${out}"; return 1; }
}

test_silent_on_knowledge_server() {
  # A publish straight to a knowledge system is not a soul staging write.
  local home; home="$(new_home with_knowledge)"
  local out; out="$(payload PostToolUse "mcp__knowledge__mark_publish" "${ADR_URL}" "# 0007. X" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence for a knowledge-system publish, got: ${out}"; return 1; }
}

test_silent_without_soul_config() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf 'knowledge\n' > "${home}/.demarkus/knowledge-systems"
  local out; out="$(payload PostToolUse "${SOUL_TOOL}" "${ADR_URL}" "# 0007. X" | HOME="${home}" bash "${HOOK}" 2>&1)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want silence with no soul configured, got: ${out}"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== promote-nudge shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
