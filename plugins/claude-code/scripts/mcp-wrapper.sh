#!/usr/bin/env bash
# mcp-wrapper.sh — launches demarkus-mcp with the right token and port.
#
# Claude Code launches MCP subprocesses with a fixed env, so the plugin can't
# hand the token over at spawn time via a hook. Instead .mcp.json invokes this
# wrapper, which reads the plugin config + token file, exports DEMARKUS_AUTH,
# and execs demarkus-mcp. The MCP binary already honors DEMARKUS_AUTH via the
# standard tokens.Resolve cascade — no core change.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

if ! load_config; then
  echo "[demarkus-memory] plugin is not configured — SessionStart hook may not have run yet" >&2
  exit 1
fi

if [[ ! -x "${PLUGIN_BIN_DIR}/demarkus-mcp" ]]; then
  echo "[demarkus-memory] demarkus-mcp not installed at ${PLUGIN_BIN_DIR}" >&2
  exit 1
fi

if [[ -f "${PLUGIN_TOKEN_FILE}" ]]; then
  DEMARKUS_AUTH="$(cat "${PLUGIN_TOKEN_FILE}")"
  export DEMARKUS_AUTH
fi

exec "${PLUGIN_BIN_DIR}/demarkus-mcp" \
  -host "mark://localhost:${PORT}" \
  -insecure \
  "$@"
