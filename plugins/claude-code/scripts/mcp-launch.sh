#!/usr/bin/env bash
# MCP launcher (memory). Claude Code connects MCP servers while the SessionStart
# hook is still bootstrapping, so a clean or freshly re-pinned machine would exec
# a missing or stale binary. Install first; stdout stays clean for MCP stdio.
set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="${HOME}/.demarkus/bin"
BIN="${BIN_DIR}/demarkus-plugin"

# An MCP command has no runtime timeout, so a hung step would hang the server
# start. Pure bash: macOS ships no timeout(1). set -m gives each job its own
# process group, so the kill reaches children (curl, demarkus-token) too.
bounded() {
  local secs="$1" rc=0 pid dog
  shift
  set -m
  # Job control skips the usual /dev/null stdin; keep jobs off the MCP pipe.
  "$@" </dev/null &
  pid=$!
  ( sleep "${secs}"; kill -- "-${pid}" 2>/dev/null ) </dev/null >/dev/null 2>&1 &
  dog=$!
  set +m
  wait "${pid}" || rc=$?
  kill -- "-${dog}" 2>/dev/null || true
  return "${rc}"
}

bounded 300 bash "${SCRIPTS_DIR}/bootstrap.sh" 1>&2 || echo "[demarkus-memory] bootstrap failed or timed out; trying the installed binary" >&2
[[ -x "${BIN}" ]] || { echo "[demarkus-memory] ${BIN} not installed; run /soul-init" >&2; exit 1; }

# mcp-serve needs the pinned demarkus-mcp and the token, which only provision
# installs. Its cross-process lock serializes this with the hook's run. Not
# fatal: a failed upgrade (offline) must not take down an installed memory.
bounded 300 "${BIN}" provision 1>&2 || echo "[demarkus-memory] provision failed or timed out; mcp-serve will report what is missing" >&2

exec "${BIN}" "$@"
