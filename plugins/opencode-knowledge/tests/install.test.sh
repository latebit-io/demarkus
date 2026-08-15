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

"${ROOT}/install.sh" >/dev/null

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

mkdir -p "${HOME}/.demarkus"
touch "${HOME}/.demarkus/knowledge-systems"
"${ROOT}/install.sh" --uninstall >/dev/null

[[ ! -e "${plugin}" ]]
[[ ! -e "${skill}" ]]
[[ ! -e "${assets}" ]]
[[ -e "${HOME}/.demarkus/knowledge-systems" ]]
