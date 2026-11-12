#!/usr/bin/env bash
# PostToolUse gate adapter. The shared demarkus-plugin binary decides; on a warn
# (the default strictness) it emits a reminder to the model and the user, otherwise
# nothing (block/ask were already enforced PreToolUse). Fails open when the
# binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
# Keep stderr visible (see gate-pre.sh): fail-open diagnostics must not be hidden.
"${BIN}" gate --format claude-post || exit 0
