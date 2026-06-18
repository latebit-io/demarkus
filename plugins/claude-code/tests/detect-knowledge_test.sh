#!/usr/bin/env bash
# Shell-level tests for detect-knowledge.sh — the promote bridge's detection
# gate. The script re-sources lib.sh in its own process, and KNOWLEDGE_REGISTRY
# is bound to HOME at source time, so each case runs the script in a fresh
# subprocess with a controlled HOME. Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
DETECT="${PLUGIN_DIR}/scripts/detect-knowledge.sh"

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

# write_registry HOME LINES... — create the knowledge-systems registry under HOME.
write_registry() {
  local home="$1"; shift
  mkdir -p "${home}/.demarkus"
  printf '%s\n' "$@" > "${home}/.demarkus/knowledge-systems"
}

test_no_registry_reports_none() {
  local home; home="$(mktemp -d)"
  local out; out="$(HOME="${home}" bash "${DETECT}" 2>&1)"
  rm -rf "${home}"
  [[ "${out}" == "NO_KNOWLEDGE" ]] || { echo "want NO_KNOWLEDGE, got: ${out}"; return 1; }
}

test_empty_registry_reports_none() {
  # A registry of only blank lines/whitespace must read as no endpoint.
  local home; home="$(mktemp -d)"
  mkdir -p "${home}/.demarkus"
  printf '\n   \n\t\n' > "${home}/.demarkus/knowledge-systems"
  local out; out="$(HOME="${home}" bash "${DETECT}" 2>&1)"
  rm -rf "${home}"
  [[ "${out}" == "NO_KNOWLEDGE" ]] || { echo "want NO_KNOWLEDGE for blank registry, got: ${out}"; return 1; }
}

test_one_endpoint_listed() {
  local home; home="$(mktemp -d)"
  write_registry "${home}" "knowledge"
  local out; out="$(HOME="${home}" bash "${DETECT}" 2>&1)"
  rm -rf "${home}"
  local ok=1
  [[ "$(printf '%s\n' "${out}" | sed -n '1p')" == "KNOWLEDGE" ]] || { echo "line 1 should be KNOWLEDGE"; ok=0; }
  [[ "$(printf '%s\n' "${out}" | sed -n '2p')" == "knowledge" ]] || { echo "line 2 should be the slug"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_multiple_endpoints_listed() {
  local home; home="$(mktemp -d)"
  write_registry "${home}" "knowledge" "team-b"
  local out; out="$(HOME="${home}" bash "${DETECT}" 2>&1)"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "KNOWLEDGE"  || { echo "missing KNOWLEDGE header"; ok=0; }
  printf '%s\n' "${out}" | grep -qx "knowledge"  || { echo "missing slug knowledge"; ok=0; }
  printf '%s\n' "${out}" | grep -qx "team-b"     || { echo "missing slug team-b"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_whitespace_stripped() {
  # Surrounding whitespace and interspersed blank lines are normalized away.
  local home; home="$(mktemp -d)"
  mkdir -p "${home}/.demarkus"
  printf '  knowledge  \n\n\tteam-b\n' > "${home}/.demarkus/knowledge-systems"
  local out; out="$(HOME="${home}" bash "${DETECT}" 2>&1)"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "knowledge" || { echo "slug not trimmed to 'knowledge'"; ok=0; }
  printf '%s\n' "${out}" | grep -qx "team-b"     || { echo "slug not trimmed to 'team-b'"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== detect-knowledge shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
