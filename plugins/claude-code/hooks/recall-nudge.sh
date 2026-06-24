#!/usr/bin/env bash
# UserPromptSubmit adapter. The shared demarkus-plugin binary decides the recall
# reminder (and reads its own config — only nudges when a soul is configured and
# the prompt reads like recall), so this just pipes the hook payload through.
# Fails open (no output) when the binary isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event recall --surface memory --format claude || exit 0
