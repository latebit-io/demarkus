#!/usr/bin/env bash
# sessionStart adapter (knowledge): bootstrap the shared binary (no local server),
# then emit knowledge guidance as Cursor additional_context. Bootstrap chatter
# goes to stderr; stdout carries only the binary JSON. Fails open.
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"

bash "${HOOK_DIR}/../scripts/bootstrap.sh" >&2 || echo "[demarkus-knowledge] bootstrap failed; gates and guidance are unavailable this session" >&2
[[ -x "${BIN}" ]] || exit 0
"${BIN}" guidance --surface knowledge --guidance-file "${GUIDANCE_FILE}" --format cursor || { echo "[demarkus-knowledge] guidance exited $?; no guidance injected" >&2; exit 0; }
