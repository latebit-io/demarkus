#!/usr/bin/env bash
# Shell-level tests for detect-promote.sh — the promote-destination detector
# (brokered knowledge systems + plain remote targets). Each case runs the script
# in a fresh subprocess with a controlled HOME (lib.sh binds its registry paths
# to HOME at source time). Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
DETECT="${PLUGIN_DIR}/scripts/detect-promote.sh"

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

# detect HOME — run the detector with a controlled HOME.
detect() { HOME="$1" bash "${DETECT}" 2>&1; }

test_no_destinations_reports_none() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  local out; out="$(detect "${home}")"
  rm -rf "${home}"
  [[ "${out}" == "NONE" ]] || { echo "want NONE, got: ${out}"; return 1; }
}

test_brokered_only() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf 'knowledge\nteam-b\n' > "${home}/.demarkus/knowledge-systems"
  local out; out="$(detect "${home}")"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "knowledge knowledge" || { echo "missing 'knowledge knowledge'"; ok=0; }
  printf '%s\n' "${out}" | grep -qx "knowledge team-b"   || { echo "missing 'knowledge team-b'"; ok=0; }
  printf '%s\n' "${out}" | grep -q  "NONE"               && { echo "should not say NONE"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_plain_targets_only() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf 'team-remote /shared/ team remote\n' > "${home}/.demarkus/promote-targets"
  local out; out="$(detect "${home}")"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "target team-remote /shared/ team remote" || { echo "missing target line"; ok=0; }
  printf '%s\n' "${out}" | grep -q  "NONE"                                     && { echo "should not say NONE"; ok=0; }
  printf '%s\n' "${out}" | grep -q  "knowledge "                               && { echo "no brokered systems expected"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_both_kinds() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf 'knowledge\n' > "${home}/.demarkus/knowledge-systems"
  printf 'team-remote /shared/\n' > "${home}/.demarkus/promote-targets"
  local out; out="$(detect "${home}")"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "knowledge knowledge"        || { echo "missing brokered line"; ok=0; }
  printf '%s\n' "${out}" | grep -qx "target team-remote /shared/" || { echo "missing target line"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

test_targets_comments_and_blanks_ignored() {
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf '# a comment\n\n   \nteam-remote /shared/\n' > "${home}/.demarkus/promote-targets"
  local out; out="$(detect "${home}")"
  rm -rf "${home}"
  local ok=1
  printf '%s\n' "${out}" | grep -qx "target team-remote /shared/" || { echo "real target missing"; ok=0; }
  printf '%s\n' "${out}" | grep -q  "comment"                     && { echo "comment leaked"; ok=0; }
  (( ok == 1 )) || { echo "got: ${out}"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== detect-promote shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
