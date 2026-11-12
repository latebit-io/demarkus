#!/usr/bin/env bash
# beforeMCPExecution gate adapter. Gate logic lives in the shared demarkus-plugin
# binary; this pipes Cursor's payload through and prints its permission verdict.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
# Absent binary fails open, with an explicit allow: the hook is failClosed, so
# empty output is not a safe verdict.
[ -x "${BIN}" ] || { echo "[demarkus] ${BIN} missing (bootstrap not run yet?); allowing the write" >&2; echo '{"permission":"allow"}'; exit 0; }
# A brand's defaults sit beside its guidance; the base plugins ship none.
DEMARKUS_BRAND_FILE="$(dirname "$0")/../context/brand.json"
export DEMARKUS_BRAND_FILE
# The binary exits 0 on its own internal errors, so non-zero is a crash or a
# broken install. Exit 2 denies: enforcement must not vanish silently.
"${BIN}" gate --format cursor || { rc=$?; echo "[demarkus] gate crashed (exit ${rc}); write blocked. Retry, or reinstall ${BIN}" >&2; exit 2; }
