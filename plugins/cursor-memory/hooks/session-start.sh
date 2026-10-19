#!/usr/bin/env bash
# sessionStart adapter (memory). Ensure the shared demarkus-plugin binary is
# present (bootstrap.sh; Cursor connects MCP servers before this hook ends, so
# the launcher cannot be waited for), reprovision only a configured, unhealthy
# setup, then emit the session guidance as Cursor's additional_context.
# Provisioning chatter goes to stderr; stdout carries only the binary's JSON.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

bash "${HOOK_DIR}/../scripts/bootstrap.sh" 1>&2 || echo "[demarkus] bootstrap failed; memory tools may be unavailable this session" >&2
[[ -x "${BIN}" ]] || exit 0
"${BIN}" provision if-configured 1>&2 || echo "[demarkus] provision exited $?; memory may be unavailable this session" >&2
"${BIN}" guidance --surface memory --guidance-file "${GUIDANCE_FILE}" --format cursor || { echo "[demarkus] guidance exited $?; no guidance injected" >&2; exit 0; }
