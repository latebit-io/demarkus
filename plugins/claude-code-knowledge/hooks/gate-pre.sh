#!/usr/bin/env bash
# PreToolUse gate adapter. All gate LOGIC (publish tag-gate, destination gate,
# knowledge axes/fields) lives in the shared demarkus-plugin binary — this just
# pipes Claude's hook payload to it and lets it emit the Claude-native decision
# (deny/ask, or nothing). Fails open (no output) when the binary isn't installed
# yet, so a not-yet-provisioned session never wrongly blocks.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" gate --format claude-pre 2>/dev/null || exit 0
