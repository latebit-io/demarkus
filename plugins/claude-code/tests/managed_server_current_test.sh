#!/usr/bin/env bash
# Shell-level tests for managed_server_current() in scripts/lib.sh.
#
# managed_server_current decides whether a recorded managed demarkus-server can
# be reused: it must be alive AND stamped with the current SERVER_VERSION pin.
# A live server on a stale or missing stamp (binary upgraded under it, or an
# older plugin that never stamped) must read as not-current so the caller
# restarts it onto the new binary — that's the "server binary behind after a
# plugin update" fix. Each case uses an isolated temp dir and a real background
# process (sleep) for liveness. Pure bash, no deps, no real server spawned.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
# shellcheck source=../scripts/lib.sh
. "${PLUGIN_DIR}/scripts/lib.sh"

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then echo "PASS  ${name}"; PASS=$((PASS + 1))
  else echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'; FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}"); fi
}

test_reuses_live_matching_stamp() {
  local tmp; tmp="$(mktemp -d)"
  sleep 30 >/dev/null 2>&1 & local pid=$!
  printf '%s\n' "${pid}" > "${tmp}/.pid"
  printf '%s\n' "${SERVER_VERSION}" > "${tmp}/.server-version"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  kill "${pid}" 2>/dev/null; rm -rf "${tmp}"
  (( rc == 0 )) || { echo "a live server stamped with the current version must be reusable"; return 1; }
}

test_restarts_live_stale_stamp() {
  local tmp; tmp="$(mktemp -d)"
  sleep 30 >/dev/null 2>&1 & local pid=$!
  printf '%s\n' "${pid}" > "${tmp}/.pid"
  printf '0.0.1-old\n' > "${tmp}/.server-version"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  kill "${pid}" 2>/dev/null; rm -rf "${tmp}"
  (( rc != 0 )) || { echo "a live server on a stale stamp must read as not-current (needs restart)"; return 1; }
}

test_restarts_live_missing_stamp() {
  # An older plugin spawned the server before stamps existed: no .server-version.
  local tmp; tmp="$(mktemp -d)"
  sleep 30 >/dev/null 2>&1 & local pid=$!
  printf '%s\n' "${pid}" > "${tmp}/.pid"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  kill "${pid}" 2>/dev/null; rm -rf "${tmp}"
  (( rc != 0 )) || { echo "a live server with no stamp must read as not-current (needs restart)"; return 1; }
}

test_not_current_when_dead_pid() {
  local tmp; tmp="$(mktemp -d)"
  sleep 30 >/dev/null 2>&1 & local pid=$!
  kill "${pid}" 2>/dev/null; wait "${pid}" 2>/dev/null
  printf '%s\n' "${pid}" > "${tmp}/.pid"
  printf '%s\n' "${SERVER_VERSION}" > "${tmp}/.server-version"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  rm -rf "${tmp}"
  (( rc != 0 )) || { echo "a dead PID must read as not-current even with a matching stamp"; return 1; }
}

test_not_current_when_no_pid_file() {
  local tmp; tmp="$(mktemp -d)"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  rm -rf "${tmp}"
  (( rc != 0 )) || { echo "a missing PID file must read as not-current"; return 1; }
}

test_not_current_when_garbage_pid() {
  # A truncated/option-like .pid must never be passed to kill as-is.
  local tmp; tmp="$(mktemp -d)"
  printf -- '-1\n' > "${tmp}/.pid"
  printf '%s\n' "${SERVER_VERSION}" > "${tmp}/.server-version"
  managed_server_current "${tmp}/.pid" "${tmp}/.server-version"; local rc=$?
  rm -rf "${tmp}"
  (( rc != 0 )) || { echo "a non-numeric PID must read as not-current"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== managed_server_current shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
