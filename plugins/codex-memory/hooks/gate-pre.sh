#!/usr/bin/env bash
# Codex PreToolUse gate adapter. All gate LOGIC (publish tag-gate, destination
# gate, knowledge axes/fields) lives in the shared demarkus-plugin binary — this
# just pipes Codex's hook payload to it and lets it emit the Codex-native
# decision (deny, or nothing). Codex's PreToolUse stdin (tool_name, tool_input,
# cwd) and output schema match Claude's, so the only harness-specific bit is the
# --format. Fails open (no output) when the binary isn't installed yet, so a
# not-yet-provisioned session never wrongly blocks.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
# Keep stderr visible: the binary logs every fail-open path there, and silencing
# it would hide a malformed payload or config error that disables enforcement.
"${BIN}" gate --format codex-pre || exit 0
