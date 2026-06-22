#!/usr/bin/env bash
# Shell-level tests for /soul-default: the lib.sh catalog helpers (soul_catalog,
# is_catalog_soul, local_soul_present) and scripts/soul-default.sh list/set.
#
# Each test_* runs in its own throwaway HOME so the real ~/.demarkus is never
# touched. Pure bash/awk — nothing to install, nothing to skip.

# shellcheck disable=SC1090
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
LIB="${PLUGIN_DIR}/scripts/lib.sh"
DEF="${PLUGIN_DIR}/scripts/soul-default.sh"

[[ -f "${LIB}" ]]  || { echo "lib.sh not found at ${LIB}"; exit 2; }
[[ -x "${DEF}" ]]  || { echo "soul-default.sh not executable at ${DEF}"; exit 2; }

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
    echo "FAIL  ${name}"
    printf '%s\n' "${out}" | sed 's/^/      /'
    FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
  fi
}

new_home() { local t; t="$(mktemp -d)"; export HOME="${t}"; echo "${t}"; }

# def — run soul-default.sh against the current HOME. Echoes stdout.
def() { bash "${DEF}" "$@"; }

# seed_local — make the local managed soul "present" by writing a minimal conf.
seed_local() { mkdir -p "${HOME}/.demarkus"; printf 'SOUL_DIR=/x\nPORT=6309\nMODE=reuse\n' > "${HOME}/.demarkus/plugin-memory.conf"; }

# ----- lib helper tests -------------------------------------------------------

test_catalog_unions_local_and_remote() {
  new_home >/dev/null; . "${LIB}"
  seed_local
  register_remote_soul "soul" "mark://soul.demarkus.io" 1 "-"
  local out; out="$(soul_catalog)"
  [[ "$(printf '%s\n' "${out}" | head -1)" == $'demarkus-memory\tlocal\t-\t-' ]] \
    || { echo "local row wrong / not first: ${out}"; return 1; }
  printf '%s\n' "${out}" | grep -qF $'soul\tremote\tmark://soul.demarkus.io\t1' \
    || { echo "remote row missing: ${out}"; return 1; }
}

test_catalog_empty_when_nothing_joined() {
  new_home >/dev/null; . "${LIB}"
  [[ -z "$(soul_catalog)" ]] || { echo "expected empty catalog"; return 1; }
}

test_catalog_remote_only_when_no_local() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  local out; out="$(soul_catalog)"
  printf '%s\n' "${out}" | grep -q 'demarkus-memory' && { echo "local row should be absent: ${out}"; return 1; }
  return 0
}

test_is_catalog_soul() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  is_catalog_soul soul             || { echo "remote slug not accepted"; return 1; }
  is_catalog_soul demarkus-memory  && { echo "local accepted with no conf"; return 1; }
  seed_local
  is_catalog_soul demarkus-memory  || { echo "local not accepted with conf"; return 1; }
  is_catalog_soul nope             && { echo "unjoined slug accepted"; return 1; }
  return 0
}

# ----- soul-default.sh tests --------------------------------------------------

test_list_empty() {
  new_home >/dev/null
  local out; out="$(def --list --bind /repo/x)"
  [[ "${out}" == "EMPTY" ]] || { echo "expected EMPTY, got: ${out}"; return 1; }
}

test_list_marks_current() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  register_remote_soul "hub"  "mark://h" 0 "-"
  bind_project_soul /repo/x soul
  local out; out="$(def --list --bind /repo/x)"
  [[ "$(printf '%s\n' "${out}" | head -1)" == "CATALOG" ]] || { echo "no CATALOG header: ${out}"; return 1; }
  printf '%s\n' "${out}" | grep -qF $'soul\tremote\tmark://s\t0\t*' || { echo "current not marked: ${out}"; return 1; }
  printf '%s\n' "${out}" | grep -qF $'hub\tremote\tmark://h\t0\t-'  || { echo "non-current marked: ${out}"; return 1; }
}

test_set_binds_and_upserts() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  register_remote_soul "hub"  "mark://h" 0 "-"
  [[ "$(def --set soul --bind /repo/x)" == "OK soul" ]] || { echo "set soul failed"; return 1; }
  [[ "$(project_soul_binding /repo/x)" == "soul" ]]     || { echo "binding not soul"; return 1; }
  [[ "$(def --set hub --bind /repo/x)" == "OK hub" ]]   || { echo "rebind failed"; return 1; }
  [[ "$(project_soul_binding /repo/x)" == "hub" ]]      || { echo "rebind not applied"; return 1; }
  local n; n="$(awk -F'\t' '$1=="/repo/x"' "${HOME}/.demarkus/project-souls" | wc -l | tr -d ' ')"
  [[ "${n}" -eq 1 ]] || { echo "expected 1 binding row, got ${n}"; return 1; }
}

test_set_local_when_configured() {
  new_home >/dev/null; . "${LIB}"
  seed_local
  [[ "$(def --set demarkus-memory --bind /repo/x)" == "OK demarkus-memory" ]] \
    || { echo "set local failed"; return 1; }
  [[ "$(project_soul_binding /repo/x)" == "demarkus-memory" ]] \
    || { echo "local binding not applied"; return 1; }
}

test_set_rejects_flaglike_slug() {
  new_home >/dev/null
  local out; out="$(def --set --bind /repo/x 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}"               || { echo "flag-like slug should FAIL: ${out}"; return 1; }
  grep -q 'requires a soul slug' <<<"${out}" || { echo "wrong message: ${out}"; return 1; }
}

test_set_unjoined_fails() {
  new_home >/dev/null
  local out; out="$(def --set ghost --bind /repo/x 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}"        || { echo "unjoined slug should FAIL: ${out}"; return 1; }
  grep -q 'soul-join' <<<"${out}"     || { echo "should point to /soul-join: ${out}"; return 1; }
}

test_requires_bind() {
  new_home >/dev/null
  local out; out="$(def --list 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}" || { echo "missing --bind should FAIL: ${out}"; return 1; }
}

test_other_project_rows_preserved() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  bind_project_soul /repo/other soul
  def --set soul --bind /repo/x >/dev/null
  [[ "$(project_soul_binding /repo/other)" == "soul" ]] || { echo "other project row clobbered"; return 1; }
}

# ----- runner -----------------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== soul-default shell tests ==="
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
