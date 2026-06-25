#!/usr/bin/env bash
# Codex PreToolUse gate adapter (knowledge). The publish tag-gate (tags +
# importance + required axes/fields) lives in the shared demarkus-plugin binary;
# this pipes Codex's hook payload to it and emits the Codex-native decision.
# Fails open (no output) when the binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
# Keep stderr visible: fail-open diagnostics must not be hidden.
"${BIN}" gate --format codex-pre || exit 0
