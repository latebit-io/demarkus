#!/usr/bin/env bash
# detect-knowledge.sh — report joined knowledge-system endpoints.
#
# The detection gate for the promote bridge (soul → knowledge). Reads the
# demarkus-knowledge plugin's registry read-only; never touches the broker.
#
# Output:
#   "NO_KNOWLEDGE"                  — no knowledge endpoint joined; promote is dormant
#   "KNOWLEDGE" followed by slugs   — one joined endpoint slug per line
#
# Used by /promote to decide whether there is anywhere to promote to.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

endpoints=$(knowledge_endpoints)
if [[ -z "${endpoints}" ]]; then
  echo "NO_KNOWLEDGE"
  exit 0
fi

echo "KNOWLEDGE"
echo "${endpoints}"
