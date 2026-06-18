#!/usr/bin/env bash
# detect-promote.sh — report every promote destination, the detection gate for
# /promote and /soul-refresh. A destination is either a brokered knowledge
# system (joined via the demarkus-knowledge plugin's /knowledge-join) or a plain
# remote demarkus server registered as a promote target. Read-only; never
# touches the broker.
#
# Output:
#   "NONE"                            — no destination of either kind; promote dormant
#   otherwise one line per destination:
#     "knowledge <slug>"              — a brokered knowledge system (slug = MCP server)
#     "target <slug> <path> [label]"  — a plain remote endpoint + its declared write path

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

found=0

while IFS= read -r slug; do
  [[ -n "${slug}" ]] || continue
  echo "knowledge ${slug}"
  found=1
done <<EOF
$(knowledge_endpoints)
EOF

while IFS= read -r entry; do
  [[ -n "${entry}" ]] || continue
  echo "target ${entry}"
  found=1
done <<EOF
$(promote_targets)
EOF

[[ "${found}" -eq 1 ]] || echo "NONE"
