#!/usr/bin/env bash
# SessionStart hook (knowledge): install or update the shared helper, then
# emit the session guidance it builds. Fails open.
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

# A sandbox (Cowork) has no claude CLI and cannot see the host's ~/.demarkus:
# nothing downloads there, the static guidance stands in.
if [[ -x "${BIN}" ]] || command -v claude >/dev/null 2>&1; then
  bash "${SCRIPTS_DIR}/bootstrap.sh" >&2 || echo "[demarkus] bootstrap failed; gates and guidance may be unavailable this session" >&2
fi
if [[ ! -x "${BIN}" ]]; then
  awk '!body && (/^<!--.*-->$/ || /^$/) { next } { body = 1; print }' "${GUIDANCE_FILE}" ||
    echo "[demarkus] reading ${GUIDANCE_FILE} failed; no guidance injected" >&2
  exit 0
fi
"${BIN}" guidance --surface knowledge --guidance-file "${GUIDANCE_FILE}" --format claude || { echo "[demarkus] guidance exited $?; no guidance injected" >&2; exit 0; }
