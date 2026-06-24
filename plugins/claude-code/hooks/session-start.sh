#!/usr/bin/env bash
# SessionStart hook (memory). Ensure the shared demarkus-plugin binary is present
# (bootstrap.sh), let it provision the managed server, then emit the session
# guidance — all the logic lives in the binary now. stdout carries only the JSON
# the binary emits for Claude Code; provisioning chatter goes to stderr.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

{
  bash "${SCRIPTS_DIR}/bootstrap.sh" || echo "[demarkus-memory] bootstrap failed; memory tools may be unavailable this session" >&2
  [[ -x "${BIN}" ]] && "${BIN}" provision || true
} 1>&2

[[ -x "${BIN}" ]] || exit 0
"${BIN}" guidance --surface memory --guidance-file "${GUIDANCE_FILE}" --format claude || exit 0
