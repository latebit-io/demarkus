#!/bin/bash
# Hand-copied plugin files must stay byte identical to their canonical copy.
# Branded hooks and the OpenCode bootstraps are not covered yet (T12).
set -euo pipefail

cd "$(dirname "$0")/.."

# canonical|copy
pairs=(
  "plugins/claude-code/scripts/bootstrap.sh|plugins/claude-code-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/cursor-memory/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/cursor-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/pi-memory/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/pi-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/hooks/gate-post.sh|plugins/claude-code-knowledge/hooks/gate-post.sh"
)

fail=0
for pair in "${pairs[@]}"; do
  canonical="${pair%%|*}"
  copy="${pair##*|}"
  if ! cmp -s "$canonical" "$copy"; then
    echo "drift: $copy differs from $canonical" >&2
    fail=1
  fi
done

[ "$fail" -eq 0 ] || exit 1
echo "✓ ${#pairs[@]} plugin copies identical"
