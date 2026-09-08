#!/usr/bin/env bash
# Stop adapter for the session-end journal nudge. The shared demarkus-plugin
# binary reads the Stop payload, scans the transcript, applies the loop guards
# (stop_hook_active, once-per-session sentinel) and emits decision:block.
# Fails open: a missing binary or a failed nudge never blocks the Stop.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
[ -x "${BIN}" ] || exit 0
"${BIN}" nudge --event session-end --format claude || {
  echo "[demarkus-memory] session-end nudge failed (exit $?); skipping" >&2
}
exit 0
