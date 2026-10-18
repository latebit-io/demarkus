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

echo "Checking layers..."
(cd tools && go run ./layercheck)

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

echo "Checking generated eval mocks..."
(cd tools && go run ./evalmocks check)

echo "✓ All checks passed"

# A hint, not a check: the eval is paid and model-judged. Shared client and
# protocol packages are left out; the eval mocks the server they back.
eval_paths=(plugins/claude-code plugins/prompt-source tools/demarkus-plugin)
if [ -n "$(git diff --name-only "$(git merge-base main HEAD)" -- "${eval_paths[@]}")$(git ls-files -o --exclude-standard -- "${eval_paths[@]}")" ]; then
  echo "Plugin changed: run make plugin-eval (paid, about \$1)"
fi
