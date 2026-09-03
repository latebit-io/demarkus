#!/usr/bin/env bash
# sessionStart adapter (memory). Ensure the shared demarkus-plugin binary is
# present (bootstrap.sh), let it provision the managed server, then emit the
# session guidance as Cursor's additional_context. Provisioning chatter goes to
# stderr; stdout carries only the JSON the binary emits.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

{
  bash "${HOOK_DIR}/../scripts/bootstrap.sh" || echo "[demarkus-memory] bootstrap failed; memory tools may be unavailable this session" >&2
  [[ -x "${BIN}" ]] && "${BIN}" provision || true
} 1>&2

[[ -x "${BIN}" ]] || exit 0
"${BIN}" guidance --surface memory --guidance-file "${GUIDANCE_FILE}" --format cursor || exit 0
