#!/usr/bin/env bash
# PreToolUse gate adapter. Gate logic lives in the shared demarkus-plugin binary;
# this pipes Claude's payload through and lets it emit deny/ask, or nothing.
set -uo pipefail
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
# Under the 20s hook timeout in plugin.json: a hook cancelled at that timeout
# fails open (Claude drops its output and allows the write). TERM at this
# bound, 5s grace, then KILL still returns before the hook is cancelled.
GATE_SECS=12
# Absent binary fails open: a not-yet-provisioned session must not block.
[ -x "${BIN}" ] || { echo "[demarkus] ${BIN} missing (bootstrap not run yet?); allowing the write" >&2; exit 0; }

# Signal a job's process group. A group already gone is the normal case, so
# only a failure against a live group is logged.
killgroup() {
  kill -0 -- "-$2" 2>/dev/null || return 0
  kill "-$1" -- "-$2" || echo "[demarkus] kill -$1 of process group $2 failed" >&2
}

# Same shape as scripts/mcp-launch.sh. set -m gives the gate its own process
# group so the kill reaches children; the job keeps this hook's stdin (payload)
# and stdout (deny/ask JSON), so no </dev/null on it.
timed_out=0
bounded() {
  local secs="$1" rc=0 pid dog start
  shift
  start=${SECONDS}
  set -m
  "$@" &
  pid=$!
  ( sleep "${secs}"; killgroup TERM "${pid}"; sleep 5; killgroup KILL "${pid}" ) </dev/null >/dev/null &
  dog=$!
  set +m
  wait "${pid}" || rc=$?
  killgroup TERM "${dog}"
  # Leader died by signal: sweep group members that ignored the TERM.
  [[ "${rc}" -le 128 ]] || killgroup KILL "${pid}"
  # Elapsed time alone classifies a timeout; a signal exit before the bound is a crash.
  (( SECONDS - start < secs )) || timed_out=1
  return "${rc}"
}

# The binary exits 0 on its own internal errors, so non-zero is a crash, a hang
# or a broken install. Exit 2 denies: enforcement must not vanish silently.
bounded "${GATE_SECS}" "${BIN}" gate --format claude-pre || {
  rc=$?
  if [[ "${timed_out}" -eq 1 ]]; then
    echo "[demarkus] gate did not finish within ${GATE_SECS}s (exit ${rc}); write blocked. Retry, or check the soul server" >&2
  else
    echo "[demarkus] gate crashed (exit ${rc}); write blocked. Retry, or reinstall ${BIN}" >&2
  fi
  exit 2
}
