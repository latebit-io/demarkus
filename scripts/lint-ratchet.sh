#!/bin/bash
# Ratchet for .golangci-ratchet.yml: findings may only shrink.
# Usage: lint-ratchet.sh [--update]
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
baseline="$root/lint-ratchet.txt"
current="$(mktemp)"
trap 'rm -f "$current"' EXIT

for mod in protocol server client tools; do
  # Exit 1 means findings, which is expected here; anything else is a failure.
  out="$(cd "$root/$mod" && golangci-lint run -c "$root/.golangci-ratchet.yml" \
    --output.text.print-issued-lines=false ./... 2>&1)" || {
    rc=$?
    if [ "$rc" -ne 1 ]; then
      echo "$out" >&2
      echo "lint-ratchet: golangci-lint failed in $mod (exit $rc)" >&2
      exit "$rc"
    fi
  }
  # Keys drop line and column so unrelated edits do not move entries.
  printf '%s\n' "$out" | grep -E '^[^ ]+\.go:[0-9]+' |
    sed -E 's/^([^:]+\.go):[0-9]+(:[0-9]+)?: /\1: /' >>"$current" || true
done

LC_ALL=C sort "$current" | uniq -c | sed -E 's/^ +//' >"$current.sorted"
mv "$current.sorted" "$current"

if [ "${1:-}" = "--update" ]; then
  cp "$current" "$baseline"
  echo "lint-ratchet: baseline updated ($(wc -l <"$baseline" | tr -d ' ') entries)"
  exit 0
fi

if ! diff -u "$baseline" "$current"; then
  echo "lint-ratchet: findings differ from lint-ratchet.txt." >&2
  echo "  '+' lines are new violations: fix them." >&2
  echo "  '-' lines are fixed: run scripts/lint-ratchet.sh --update." >&2
  exit 1
fi
echo "✓ lint ratchet holds ($(wc -l <"$baseline" | tr -d ' ') entries)"
