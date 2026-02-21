#!/bin/bash
set -e

echo "Running code formatting and linting..."
make fmt
make vet
golangci-lint run ./...

echo "✓ All checks passed"
