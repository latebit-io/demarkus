#!/usr/bin/env bash
# SessionStart hook (memory). scripts/mcp-launch.sh installs the binaries and
# provisions on the host; this hook reprovisions under the same lock, then emits
# the session guidance the binary builds. stdout carries only that context.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
CONF="${HOME}/.demarkus/plugin-memory.conf"

# No config (empty counts as none): a sandbox that cannot see the host's
# ~/.demarkus (Cowork), or the launcher's first run still going. Provisioning
# here would start a second soul.
if [[ ! -x "${BIN}" || ! -s "${CONF}" ]]; then
  awk '!body && (/^<!--.*-->$/ || /^$/) { next } { body = 1; print }' "${GUIDANCE_FILE}" ||
    echo "[demarkus] reading ${GUIDANCE_FILE} failed; no guidance injected" >&2
  exit 0
fi

"${BIN}" provision 1>&2 || echo "[demarkus] provision exited $?; memory may be unavailable this session" >&2
"${BIN}" guidance --surface memory --guidance-file "${GUIDANCE_FILE}" --format claude || { echo "[demarkus] guidance exited $?; no guidance injected" >&2; exit 0; }
