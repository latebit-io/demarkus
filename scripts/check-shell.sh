#!/bin/bash
# Syntax and shellcheck gate over every tracked shell script.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v shellcheck &>/dev/null; then
  echo "check-shell: shellcheck is not installed" >&2
  exit 1
fi

scripts=()
while IFS= read -r f; do scripts+=("$f"); done < <(git ls-files '*.sh')

for f in "${scripts[@]}"; do
  bash -n "$f"
done

# SC2034 stays off until the installers' unused reads are resolved (T33).
shellcheck -S warning -e SC2034 "${scripts[@]}"
echo "✓ ${#scripts[@]} shell scripts pass"
