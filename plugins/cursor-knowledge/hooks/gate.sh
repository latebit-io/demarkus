#!/usr/bin/env bash
# beforeMCPExecution gate adapter (knowledge). Gate logic lives in the shared
# demarkus-plugin binary; this pipes Cursor's payload through and prints its
# permission verdict. Fails open (no output, stderr diagnostic) when the binary is absent.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || { echo "[demarkus-knowledge] ${BIN} missing (bootstrap not run yet?); allowing the write (fail-open)" >&2; exit 0; }
"${BIN}" gate --format cursor || { echo "[demarkus-knowledge] gate exited $?; allowing the write (fail-open)" >&2; exit 0; }
