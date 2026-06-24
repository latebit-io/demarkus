#!/usr/bin/env bash
# UserPromptSubmit adapter (knowledge). The shared demarkus-plugin binary decides
# the recall reminder for the knowledge surface (only when a knowledge system is
# joined and the prompt reads like an org/shared recall), so this just pipes the
# payload through. Fails open when the binary isn't installed.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event recall --surface knowledge --format claude || exit 0
