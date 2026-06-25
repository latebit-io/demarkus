#!/usr/bin/env bash
# Codex PostToolUse gate adapter (knowledge). On a warn the binary emits an
# additionalContext reminder; block/ask were already enforced PreToolUse. Fails
# open when the binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" gate --format codex-post || exit 0
