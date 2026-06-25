#!/usr/bin/env bash
# Codex UserPromptSubmit adapter (knowledge). The binary decides the recall
# reminder (only when a knowledge system is joined and the prompt reads like
# recall), so this pipes the hook payload through. Fails open when the binary
# isn't installed yet.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event recall --surface knowledge --format codex || exit 0
