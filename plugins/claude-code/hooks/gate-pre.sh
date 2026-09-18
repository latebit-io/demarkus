#!/usr/bin/env bash
# PreToolUse gate adapter. Gate logic lives in the shared demarkus-plugin binary;
# this pipes Claude's payload through and lets it emit deny/ask, or nothing.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
# Absent binary fails open: a not-yet-provisioned session must not block.
[ -x "${BIN}" ] || { echo "[demarkus-memory] ${BIN} missing (bootstrap not run yet?); allowing the write" >&2; exit 0; }
# The binary exits 0 on its own internal errors, so non-zero is a crash or a
# broken install. Exit 2 denies: enforcement must not vanish silently.
"${BIN}" gate --format claude-pre || { rc=$?; echo "[demarkus-memory] gate crashed (exit ${rc}); write blocked. Retry, or reinstall ${BIN}" >&2; exit 2; }
