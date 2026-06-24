#!/usr/bin/env bash
# PostToolUse adapter. The shared demarkus-plugin binary decides the ADR promote
# nudge (checks promote destination, local-soul scope, the /adr/*.md path, and
# the already-promoted marker), so this just pipes the publish payload through.
# Fails open (no output) when the binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event promote --format claude || exit 0
