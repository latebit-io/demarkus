#!/usr/bin/env bash
# MCP launcher (memory). Claude Code connects MCP servers while the SessionStart
# hook is still bootstrapping, so a clean or freshly re-pinned machine would exec
# a missing or stale binary. Install first; stdout stays clean for MCP stdio.
set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="${HOME}/.demarkus/bin"
BIN="${BIN_DIR}/demarkus-plugin"

bash "${SCRIPTS_DIR}/bootstrap.sh" 1>&2 || echo "[demarkus-memory] bootstrap failed; trying the installed binary" >&2
[[ -x "${BIN}" ]] || { echo "[demarkus-memory] ${BIN} not installed; run /soul-init" >&2; exit 1; }

# mcp-serve needs the pinned demarkus-mcp and the token, which only provision
# installs. Its cross-process lock serializes this with the hook's run. Not
# fatal: a failed upgrade (offline) must not take down an installed memory.
"${BIN}" provision 1>&2 || echo "[demarkus-memory] provision failed; mcp-serve will report what is missing" >&2

exec "${BIN}" "$@"
