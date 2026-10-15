#!/usr/bin/env bash
# Runs the Claude Code memory plugin's eval suite: the shipped plugin is
# copied beside this suite (claude plugin eval reads evals/ under the plugin)
# so no eval file ships with the plugin. Extra arguments go to the eval.
set -euo pipefail

cd "$(dirname "$0")/../../.."
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
cp -R plugins/claude-code/. "${work}/"
cp -R tools/demarkus-plugin/evals "${work}/evals"
rm -rf "${work}/evals/results"
mkdir -p tools/demarkus-plugin/evals/results
claude plugin eval --trust-plugin --no-publish --ablation none \
  --output-dir "tools/demarkus-plugin/evals/results/$(date +%Y%m%dT%H%M%S)" "$@" "${work}"
