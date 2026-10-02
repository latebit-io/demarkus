#!/bin/bash
set -e

echo "Checking formatting..."
# ls-files still lists a tracked file deleted but not yet staged; skip it.
unformatted=$(git ls-files -co --exclude-standard "*.go" | while read -r f; do [ -f "$f" ] && echo "$f"; done | xargs gofmt -l)
if [ -n "$unformatted" ]; then
  echo "Not gofmt clean (run make fmt):" >&2
  echo "$unformatted" >&2
  exit 1
fi

make vet
make test

make lint

echo "Checking lint ratchet..."
bash scripts/lint-ratchet.sh

echo "Checking comment length on changed code..."
bash scripts/check-comment-length.sh

echo "Checking shell scripts..."
bash scripts/check-shell.sh

echo "Checking plugin copies..."
bash scripts/check-identical-copies.sh

echo "Checking session start hooks..."
bash scripts/test-session-start-hooks.sh

echo "Checking hook commands under a spaced plugin root..."
bash scripts/test-hook-commands.sh

echo "Checking generated plugin prompts..."
(cd tools && go run ./plugin-prompts check)

echo "✓ All checks passed"
