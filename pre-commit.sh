#!/bin/bash
set -e

echo "Running code formatting and linting..."
make fmt
make vet

for mod in protocol server client; do
  echo "Linting ${mod}..."
  (cd "$mod" && golangci-lint run ./...)
done

echo "✓ All checks passed"
