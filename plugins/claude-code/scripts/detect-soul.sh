#!/usr/bin/env bash
# detect-soul.sh — report running demarkus-server processes.
#
# Output:
#   "NO_SERVER"                       — nothing detected
#   "SERVERS" followed by one row     — one running server; row is "PID PORT ROOT"
#   "SERVERS" followed by many rows   — multiple running servers
#
# Used by /soul-init to decide whether to offer the "reuse" path.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

results=$(find_running_demarkus)
if [[ -z "${results}" ]]; then
  echo "NO_SERVER"
  exit 0
fi

echo "SERVERS"
echo "${results}"
