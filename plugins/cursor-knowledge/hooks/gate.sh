#!/usr/bin/env bash
# beforeMCPExecution gate adapter (knowledge). All gate LOGIC (tags, policy
# axes and fields, registered-system scope) lives in the shared demarkus-plugin
# binary; this pipes Cursor's payload through and lets it emit the permission
# (deny/ask, or allow with a warning agent_message). Fails open when the binary
# isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" gate --format cursor || { echo "[demarkus-knowledge] gate exited $?; allowing the write (fail-open)" >&2; exit 0; }
