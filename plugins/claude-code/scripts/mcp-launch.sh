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

# mcp-serve needs demarkus-mcp and the token, which only provision installs.
# provision takes a cross-process lock, so it serializes with the hook's run.
if [[ ! -x "${BIN_DIR}/demarkus-mcp" ]]; then
  "${BIN}" provision 1>&2 || echo "[demarkus-memory] provision failed; mcp-serve will report what is missing" >&2
fi

exec "${BIN}" "$@"
