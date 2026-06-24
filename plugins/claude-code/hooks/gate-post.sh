#!/usr/bin/env bash
# PostToolUse gate adapter. The shared demarkus-plugin binary decides; on a warn
# (the default strictness) it emits an additionalContext reminder, otherwise
# nothing (block/ask were already enforced PreToolUse). Fails open when the
# binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" gate --format claude-post 2>/dev/null || exit 0
