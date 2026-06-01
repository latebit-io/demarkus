#!/usr/bin/env bash
# Shell-level tests for seed_doc() in scripts/lib.sh.
#
# seed_doc copies a bundled seed file into the soul only when the target is
# absent — it must never clobber a user's customized doc. Each case uses an
# isolated temp dir. Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
# shellcheck source=../scripts/lib.sh
. "${PLUGIN_DIR}/scripts/lib.sh"

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

test_seeds_when_absent() {
  local tmp; tmp="$(mktemp -d)"
  printf 'SEED BODY\n' > "${tmp}/seed.md"
  seed_doc "${tmp}/seed.md" "${tmp}/soul/index.md" 2>/dev/null
  # soul dir doesn't exist yet → seed_doc is a no-op (dir not present).
  [[ ! -f "${tmp}/soul/index.md" ]] || { echo "should not seed into a missing dir"; rm -rf "${tmp}"; return 1; }
  mkdir -p "${tmp}/soul"
  seed_doc "${tmp}/seed.md" "${tmp}/soul/index.md" 2>/dev/null
  # ...and now that the dir exists, the seed lands with the right content.
  local ok=1
  [[ -f "${tmp}/soul/index.md" ]] || { echo "should seed once the target dir exists"; ok=0; }
  grep -q 'SEED BODY' "${tmp}/soul/index.md" 2>/dev/null || { echo "seeded content mismatch"; ok=0; }
  rm -rf "${tmp}"
  (( ok == 1 ))
}

test_seeds_into_writable_dir() {
  local tmp; tmp="$(mktemp -d)"
  printf 'SEED BODY\n' > "${tmp}/seed.md"
  mkdir -p "${tmp}/soul"
  seed_doc "${tmp}/seed.md" "${tmp}/soul/template.md" 2>/dev/null
  local ok=0
  [[ -f "${tmp}/soul/template.md" ]] && grep -q 'SEED BODY' "${tmp}/soul/template.md" && ok=1
  rm -rf "${tmp}"
  (( ok == 1 )) || { echo "seed_doc should have copied the seed into the writable soul dir"; return 1; }
}

test_does_not_clobber_existing() {
  local tmp; tmp="$(mktemp -d)"
  printf 'SEED BODY\n' > "${tmp}/seed.md"
  mkdir -p "${tmp}/soul"
  printf 'USER CUSTOMIZED\n' > "${tmp}/soul/template.md"
  seed_doc "${tmp}/seed.md" "${tmp}/soul/template.md" 2>/dev/null
  local keptuser=0
  grep -q 'USER CUSTOMIZED' "${tmp}/soul/template.md" && ! grep -q 'SEED BODY' "${tmp}/soul/template.md" && keptuser=1
  rm -rf "${tmp}"
  (( keptuser == 1 )) || { echo "seed_doc must not clobber an existing (user-customized) doc"; return 1; }
}

test_noop_when_seed_missing() {
  local tmp; tmp="$(mktemp -d)"
  mkdir -p "${tmp}/soul"
  seed_doc "${tmp}/nope.md" "${tmp}/soul/template.md" 2>/dev/null
  local absent=0
  [[ ! -f "${tmp}/soul/template.md" ]] && absent=1
  rm -rf "${tmp}"
  (( absent == 1 )) || { echo "seed_doc should be a no-op when the seed file is missing"; return 1; }
}

test_bundled_seeds_exist() {
  # The two docs session-start seeds must actually ship in the plugin.
  [[ -f "${PLUGIN_DIR}/seed/index.md" ]] || { echo "seed/index.md missing"; return 1; }
  [[ -f "${PLUGIN_DIR}/seed/project-template.md" ]] || { echo "seed/project-template.md missing"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== seed_doc shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
