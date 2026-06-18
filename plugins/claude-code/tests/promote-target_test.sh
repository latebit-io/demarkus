#!/usr/bin/env bash
# Shell-level tests for promote-target.sh — the plain-remote promote-target
# registry helper (add/list). Run in fresh subprocesses with a controlled HOME.
# Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
PT="${PLUGIN_DIR}/scripts/promote-target.sh"

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

pt() { HOME="$1" bash "${PT}" "${@:2}"; }

test_add_then_list() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  pt "${home}" add team-remote /shared/ "team remote" >/dev/null 2>&1
  local out; out="$(pt "${home}" list 2>/dev/null)"
  rm -rf "${home}"
  [[ "${out}" == "team-remote /shared/ team remote" ]] || { echo "list: got '${out}'"; return 1; }
}

test_add_rejects_relative_path() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local rc=0
  pt "${home}" add slug relative-path >/dev/null 2>&1 || rc=$?
  rm -rf "${home}"
  (( rc != 0 )) || { echo "a relative path should be rejected"; return 1; }
}

test_add_rejects_missing_slug() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local rc=0
  pt "${home}" add >/dev/null 2>&1 || rc=$?
  rm -rf "${home}"
  (( rc != 0 )) || { echo "missing slug should be rejected"; return 1; }
}

test_add_rejects_whitespace_path() {
  # The registry is space-delimited; a path with whitespace would corrupt parsing.
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local rc=0
  pt "${home}" add slug "/team docs" >/dev/null 2>&1 || rc=$?
  local n; n="$(pt "${home}" list 2>/dev/null | wc -l | tr -d ' ')"
  rm -rf "${home}"
  (( rc != 0 )) || { echo "a path with whitespace should be rejected"; return 1; }
  [[ "${n}" == "0" ]] || { echo "nothing should have been registered, got ${n} entries"; return 1; }
}

test_add_rejects_invalid_slug_chars() {
  # The slug is used as an MCP server name (mcp__<slug>__mark_*); guard the charset.
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local rc=0
  pt "${home}" add 'bad/slug' /shared/ >/dev/null 2>&1 || rc=$?
  rm -rf "${home}"
  (( rc != 0 )) || { echo "a slug with invalid characters should be rejected"; return 1; }
}

test_add_is_idempotent_on_slug_path() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  pt "${home}" add team-remote /shared/ >/dev/null 2>&1
  pt "${home}" add team-remote /shared/ >/dev/null 2>&1
  local n; n="$(pt "${home}" list 2>/dev/null | grep -c '^team-remote /shared/')"
  rm -rf "${home}"
  [[ "${n}" == "1" ]] || { echo "want 1 entry after duplicate add, got ${n}"; return 1; }
}

test_distinct_paths_coexist() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  pt "${home}" add team-remote /shared/ >/dev/null 2>&1
  pt "${home}" add team-remote /docs/ >/dev/null 2>&1
  local n; n="$(pt "${home}" list 2>/dev/null | grep -c '^team-remote ')"
  rm -rf "${home}"
  [[ "${n}" == "2" ]] || { echo "distinct paths should both register, got ${n}"; return 1; }
}

test_list_empty_when_none() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local out; out="$(pt "${home}" list 2>/dev/null)"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "want empty list, got: ${out}"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== promote-target shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
