#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_HOME="$(mktemp -d)"
trap 'rm -rf "${TEST_HOME}"' EXIT

export HOME="${TEST_HOME}"
export XDG_CONFIG_HOME="${TEST_HOME}/config"

expect_failure() {
  local expected="$1" output status
  shift
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if [[ ${status} -eq 0 ]]; then
    echo "expected command failure containing: ${expected}" >&2
    exit 1
  fi
  if [[ "${output}" != *"${expected}"* ]]; then
    echo "failure output missing marker: ${expected}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  fi
}

path_absent() {
  [[ ! -e "$1" && ! -L "$1" ]]
}

snapshot_state() {
  local snapshot="$1"
  rm -rf "${snapshot}"
  mkdir -p "${snapshot}"
  cp "${plugin}" "${snapshot}/plugin"
  cp -R "${skill_dir}" "${snapshot}/skill"
  cp -R "${assets}" "${snapshot}/assets"
}

assert_state_matches() {
  local snapshot="$1"
  diff -r "${snapshot}/plugin" "${plugin}"
  diff -r "${snapshot}/skill" "${skill_dir}"
  diff -r "${snapshot}/assets" "${assets}"
}

assert_no_transaction_artifacts() {
  local pattern
  for pattern in "${assets}.prev.*" "${assets}.staging.*" \
    "${plugin}.prev.*" "${plugin}.new.*" \
    "${skill_dir}.prev.*" "${skill_dir}.new.*" \
    "${assets}.uninstall.*" "${plugin}.uninstall.*" \
    "${XDG_CONFIG_HOME}/opencode/skills/.knowledge-promote.uninstall.*"; do
    if compgen -G "${pattern}" >/dev/null; then
      echo "transaction artifact remains: ${pattern}" >&2
      exit 1
    fi
  done
}

mkdir -p "${XDG_CONFIG_HOME}/opencode/plugins"
printf '%s\n' '// legacy adapter' >"${XDG_CONFIG_HOME}/opencode/plugins/demarkus-memory.ts"
expect_failure "update demarkus-opencode-memory first" "${ROOT}/install.sh"
rm "${XDG_CONFIG_HOME}/opencode/plugins/demarkus-memory.ts"

lock="${HOME}/.demarkus/opencode-knowledge.install.lock"
mkdir "${lock}"
expect_failure "another installer or stale lock exists" "${ROOT}/install.sh"
rm -rf "${lock}"

mkdir "${lock}"
expect_failure "another installer or stale lock exists" "${ROOT}/install.sh"
rm -rf "${lock}"
"${ROOT}/install.sh" >/dev/null
path_absent "${lock}"

plugin="${XDG_CONFIG_HOME}/opencode/plugins/demarkus-knowledge.ts"
skill_dir="${XDG_CONFIG_HOME}/opencode/skills/knowledge-promote"
skill="${skill_dir}/SKILL.md"
assets="${HOME}/.demarkus/opencode-knowledge"

[[ -f "${plugin}" && -s "${plugin}" ]]
[[ -f "${skill}" && -s "${skill}" ]]
[[ -f "${assets}/package.json" && -s "${assets}/package.json" ]]
[[ -f "${assets}/scripts/bootstrap.sh" && -x "${assets}/scripts/bootstrap.sh" ]]
[[ -f "${assets}/commands/knowledge-join.md" && -s "${assets}/commands/knowledge-join.md" ]]
assert_no_transaction_artifacts

# Updating from the same checkout must stay idempotent.
snapshot="${TEST_HOME}/snapshot"
snapshot_state "${snapshot}"
"${ROOT}/install.sh" >/dev/null
assert_state_matches "${snapshot}"
assert_no_transaction_artifacts

# A failure on the final adapter swap must restore all three old components.
printf '%s\n' '// previous plugin' >>"${plugin}"
printf '%s\n' '<!-- previous skill -->' >>"${skill}"
printf '%s\n' 'previous assets' >>"${assets}/package.json"
snapshot_state "${snapshot}"
REAL_MV="$(command -v mv)"
REAL_RM="$(command -v rm)"
export REAL_MV
export REAL_RM
expect_failure "deploying plugin failed" env PATH="${ROOT}/tests/failing-bin:${PATH}" "${ROOT}/install.sh"
assert_state_matches "${snapshot}"
assert_no_transaction_artifacts

snapshot_state "${snapshot}"
expect_failure "moving skill to backup failed" env FAIL_MV_MODE=uninstall \
  PATH="${ROOT}/tests/failing-bin:${PATH}" "${ROOT}/install.sh" --uninstall
assert_state_matches "${snapshot}"
assert_no_transaction_artifacts

snapshot_state "${snapshot}"
expect_failure "moving assets to backup failed" env FAIL_MV_MODE=uninstall-assets \
  PATH="${ROOT}/tests/failing-bin:${PATH}" "${ROOT}/install.sh" --uninstall
assert_state_matches "${snapshot}"
assert_no_transaction_artifacts

expect_failure "backup cleanup failed" env FAIL_RM_MODE=uninstall-backup \
  PATH="${ROOT}/tests/failing-bin:${PATH}" "${ROOT}/install.sh" --uninstall
path_absent "${plugin}"
path_absent "${skill_dir}"
path_absent "${assets}"
for leftover in "${plugin}.uninstall."* \
  "${XDG_CONFIG_HOME}/opencode/skills/.knowledge-promote.uninstall."* \
  "${assets}.uninstall."*; do
  [[ -e "${leftover}" || -L "${leftover}" ]] || continue
  "${REAL_RM}" -rf "${leftover}"
done
assert_no_transaction_artifacts

mkdir -p "${HOME}/.demarkus"
touch "${HOME}/.demarkus/knowledge-systems"
"${ROOT}/install.sh" --uninstall >/dev/null

path_absent "${plugin}"
path_absent "${skill_dir}"
path_absent "${assets}"
[[ -e "${HOME}/.demarkus/knowledge-systems" ]]
assert_no_transaction_artifacts
