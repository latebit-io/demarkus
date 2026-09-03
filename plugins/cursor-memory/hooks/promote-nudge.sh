#!/usr/bin/env bash
# beforeMCPExecution adapter for the ADR promote nudge. Cursor's after-call hook
# cannot speak to the agent, so the nudge rides the pre-call hook (the binary
# only needs the publish path and the promote-target config). Fails open.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event promote --format cursor || exit 0
