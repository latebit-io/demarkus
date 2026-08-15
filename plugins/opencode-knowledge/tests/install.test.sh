#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_HOME="$(mktemp -d)"
trap 'rm -rf "${TEST_HOME}"' EXIT

export HOME="${TEST_HOME}"
export XDG_CONFIG_HOME="${TEST_HOME}/config"

mkdir -p "${XDG_CONFIG_HOME}/opencode/plugins"
printf '%s\n' '// legacy adapter' >"${XDG_CONFIG_HOME}/opencode/plugins/demarkus-memory.ts"
if "${ROOT}/install.sh" >/dev/null 2>&1; then
  echo "legacy memory adapter should block installation" >&2
  exit 1
fi
rm "${XDG_CONFIG_HOME}/opencode/plugins/demarkus-memory.ts"

lock="${HOME}/.demarkus/opencode-knowledge.install.lock"
mkdir "${lock}"
if "${ROOT}/install.sh" >/dev/null 2>&1; then
  echo "active installer lock should block installation" >&2
  exit 1
fi
rm -rf "${lock}"

mkdir "${lock}"
if "${ROOT}/install.sh" >/dev/null 2>&1; then
  echo "stale installer lock should require explicit cleanup" >&2
  exit 1
fi
rm -rf "${lock}"
"${ROOT}/install.sh" >/dev/null
[[ ! -e "${lock}" ]]

plugin="${XDG_CONFIG_HOME}/opencode/plugins/demarkus-knowledge.ts"
skill="${XDG_CONFIG_HOME}/opencode/skills/knowledge-promote/SKILL.md"
assets="${HOME}/.demarkus/opencode-knowledge"

[[ -s "${plugin}" ]]
[[ -s "${skill}" ]]
[[ -s "${assets}/package.json" ]]
[[ -x "${assets}/scripts/bootstrap.sh" ]]
[[ -s "${assets}/commands/knowledge-join.md" ]]

# Updating from the same checkout must stay idempotent.
"${ROOT}/install.sh" >/dev/null
[[ -s "${plugin}" && -s "${assets}/context/session-guidance.md" ]]

# A failure on the final adapter swap must restore all three old components.
printf '%s\n' '// previous plugin' >>"${plugin}"
printf '%s\n' '<!-- previous skill -->' >>"${skill}"
printf '%s\n' 'previous assets' >>"${assets}/package.json"
before="$(cksum "${plugin}" "${skill}" "${assets}/package.json")"
REAL_MV="$(command -v mv)"
export REAL_MV
if PATH="${ROOT}/tests/failing-bin:${PATH}" "${ROOT}/install.sh" >/dev/null 2>&1; then
  echo "injected adapter deployment failure should fail installation" >&2
  exit 1
fi
after="$(cksum "${plugin}" "${skill}" "${assets}/package.json")"
[[ "${before}" == "${after}" ]]
if compgen -G "${assets}.prev.*" >/dev/null ||
  compgen -G "${plugin}.prev.*" >/dev/null ||
  compgen -G "${skill}.prev.*" >/dev/null; then
  echo "rollback left backup files behind" >&2
  exit 1
fi

mkdir -p "${HOME}/.demarkus"
touch "${HOME}/.demarkus/knowledge-systems"
"${ROOT}/install.sh" --uninstall >/dev/null

[[ ! -e "${plugin}" ]]
[[ ! -e "${skill}" ]]
[[ ! -e "${assets}" ]]
[[ -e "${HOME}/.demarkus/knowledge-systems" ]]
