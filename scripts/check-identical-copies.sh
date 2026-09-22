#!/bin/bash
# Hand-copied plugin files must stay identical to their canonical copy: byte
# for byte, modulo the plugin label and surface flag for the branded hooks, or
# one function block for the TS adapters. Session-start hooks differ by design
# (memory provisions a server) and are covered by test-session-start-hooks.sh.
set -euo pipefail

cd "$(dirname "$0")/.."

# canonical|copy, byte identical
pairs=(
  "plugins/claude-code/scripts/bootstrap.sh|plugins/claude-code-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/cursor-memory/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/cursor-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/opencode-memory/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/opencode-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/pi-memory/scripts/bootstrap.sh"
  "plugins/claude-code/scripts/bootstrap.sh|plugins/pi-knowledge/scripts/bootstrap.sh"
  "plugins/claude-code/hooks/gate-post.sh|plugins/claude-code-knowledge/hooks/gate-post.sh"
)

# memory hook|knowledge hook, identical once the label and surface are normalized
labeled_pairs=(
  "plugins/claude-code/hooks/gate-pre.sh|plugins/claude-code-knowledge/hooks/gate-pre.sh"
  "plugins/claude-code/hooks/recall-nudge.sh|plugins/claude-code-knowledge/hooks/recall-nudge.sh"
  "plugins/cursor-memory/hooks/gate.sh|plugins/cursor-knowledge/hooks/gate.sh"
)

# canonical|copy|function, the named TS function block identical
block_pairs=(
  "plugins/pi-memory/src/plugin.ts|plugins/pi-knowledge/src/plugin.ts|runBin"
  "plugins/opencode-memory/src/demarkus-memory.ts|plugins/opencode-knowledge/src/demarkus-knowledge.ts|runBin"
)

normalize() { sed 's/demarkus-knowledge/demarkus-memory/g; s/--surface knowledge/--surface memory/g' "$1"; }
block() { awk -v fn="$2" '$0 ~ "^(export )?(async )?function "fn"[<(]" {p=1} p {print} p && /^}/ {p=0}' "$1"; }

fail=0
for pair in "${pairs[@]}"; do
  canonical="${pair%%|*}"
  copy="${pair##*|}"
  if ! cmp -s "$canonical" "$copy"; then
    echo "drift: $copy differs from $canonical" >&2
    fail=1
  fi
done
for pair in "${labeled_pairs[@]}"; do
  canonical="${pair%%|*}"
  copy="${pair##*|}"
  if ! cmp -s <(normalize "$canonical") <(normalize "$copy"); then
    echo "drift: $copy differs from $canonical beyond the plugin label" >&2
    fail=1
  fi
done
for triple in "${block_pairs[@]}"; do
  IFS='|' read -r canonical copy fn <<<"$triple"
  if [ -z "$(block "$canonical" "$fn")" ]; then
    echo "drift: $fn not found in $canonical" >&2
    fail=1
  elif ! cmp -s <(block "$canonical" "$fn") <(block "$copy" "$fn"); then
    echo "drift: $fn in $copy differs from $canonical" >&2
    fail=1
  fi
done

[ "$fail" -eq 0 ] || exit 1
echo "✓ $((${#pairs[@]} + ${#labeled_pairs[@]} + ${#block_pairs[@]})) plugin copies identical"
