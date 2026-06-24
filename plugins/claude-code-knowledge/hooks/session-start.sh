#!/usr/bin/env bash
# SessionStart hook (knowledge). Ensure the shared demarkus-plugin binary is
# present (bootstrap.sh — this plugin owns no server, just needs the binary for
# gate/guidance), then emit the knowledge session guidance. Fails open.
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

bash "${SCRIPTS_DIR}/bootstrap.sh" >&2 || true
[[ -x "${BIN}" ]] || exit 0
"${BIN}" guidance --surface knowledge --guidance-file "${GUIDANCE_FILE}" --format claude || exit 0
