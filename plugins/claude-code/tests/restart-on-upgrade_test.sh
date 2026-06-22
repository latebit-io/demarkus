#!/usr/bin/env bash
# Tests for lib.sh restart_local_server_on_upgrade — the "restart the local
# server with its own config after a binary swap" behavior.
#
# ensure_managed_server and pid_of_server_at_root are stubbed AFTER sourcing
# lib.sh so no real server is spawned and no real process is scanned; the stubs
# are inherited by the helper's subshell. Each test runs in its own throwaway
# HOME. Pure bash — nothing to install.

# Lib path is computed at runtime (${LIB}); the sourced file is checked on its
# own. SC2034: DEMARKUS_BINARIES_REPLACED is set here but read inside lib.sh's
# restart_local_server_on_upgrade, which shellcheck can't follow across the
# runtime source.
# shellcheck disable=SC1090,SC2034
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
LIB="${PLUGIN_DIR}/scripts/lib.sh"
[[ -f "${LIB}" ]] || { echo "lib.sh not found at ${LIB}"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then
    echo "PASS  ${name}"; PASS=$((PASS + 1))
  else
    echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'
    FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
  fi
}

new_home() { local t; t="$(mktemp -d)"; export HOME="${t}"; echo "${t}"; }

# ----- tests ------------------------------------------------------------------

test_noop_when_nothing_replaced() {
  new_home >/dev/null; . "${LIB}"
  save_config "${HOME}/.demarkus/soul" 6310 default
  local mark="${HOME}/called"
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=0
  restart_local_server_on_upgrade
  [[ ! -f "${mark}" ]] || { echo "must not restart when no binary was replaced"; return 1; }
}

test_noop_when_no_config() {
  new_home >/dev/null; . "${LIB}"   # no save_config → load_config fails
  local mark="${HOME}/called"
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=1
  restart_local_server_on_upgrade
  [[ ! -f "${mark}" ]] || { echo "must not restart when no local soul is configured"; return 1; }
}

test_managed_restarts_with_recorded_config() {
  new_home >/dev/null; . "${LIB}"
  save_config "${HOME}/.demarkus/soul" 6310 default
  mkdir -p "${HOME}/.demarkus/soul"
  echo 4242 > "${HOME}/.demarkus/soul/.pid"   # our managed server is recorded
  local mark="${HOME}/called"
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=1
  restart_local_server_on_upgrade
  [[ -f "${mark}" ]] || { echo "managed server not restarted on upgrade"; return 1; }
  grep -qF "${HOME}/.demarkus/soul 6310" "${mark}" \
    || { echo "restarted with wrong config: $(cat "${mark}")"; return 1; }
}

test_reuse_stale_pid_healthy_left_alone() {
  new_home >/dev/null; . "${LIB}"
  save_config "${HOME}/.demarkus/content" 6309 reuse
  mkdir -p "${HOME}/.demarkus/content"
  echo 4242 > "${HOME}/.demarkus/content/.pid"   # stale .pid under a reuse config
  # A healthy external server the plugin does not own.
  sleep 30 &
  local sp=$!
  pid_of_server_at_root() { echo "${sp}"; }
  local mark="${HOME}/called"
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=1
  restart_local_server_on_upgrade
  # The stale .pid must NOT route into the managed-restart path: healthy server
  # left running, no respawn.
  local rc=0
  kill -0 "${sp}" 2>/dev/null || { echo "reuse + stale .pid killed a healthy external server"; rc=1; }
  [[ -f "${mark}" ]] && { echo "reuse + stale .pid must not restart via ensure_managed_server"; rc=1; }
  kill "${sp}" 2>/dev/null || true
  return "${rc}"
}

test_reuse_no_external_process_respawns() {
  new_home >/dev/null; . "${LIB}"
  save_config "${HOME}/.demarkus/content" 6309 reuse
  local mark="${HOME}/called"
  pid_of_server_at_root() { echo ""; }            # nothing running at root
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=1
  restart_local_server_on_upgrade
  [[ -f "${mark}" ]] || { echo "reuse mode should also restart on upgrade"; return 1; }
  grep -qF "${HOME}/.demarkus/content 6309" "${mark}" \
    || { echo "reuse restarted with wrong config: $(cat "${mark}")"; return 1; }
}

test_reuse_healthy_external_left_alone() {
  new_home >/dev/null; . "${LIB}"
  save_config "${HOME}/.demarkus/content" 6309 reuse
  # A stand-in for the user's healthy externally-started server.
  sleep 30 &
  local sp=$!
  pid_of_server_at_root() { echo "${sp}"; }
  local mark="${HOME}/called"
  ensure_managed_server() { echo "$*" >"${mark}"; }
  DEMARKUS_BINARIES_REPLACED=1
  restart_local_server_on_upgrade
  # A healthy server we don't own must NOT be killed and must NOT be respawned.
  local rc=0
  kill -0 "${sp}" 2>/dev/null || { echo "healthy external server was killed"; rc=1; }
  [[ -f "${mark}" ]] && { echo "must not respawn over a healthy external server"; rc=1; }
  kill "${sp}" 2>/dev/null || true   # cleanup
  return "${rc}"
}

# ----- runner -----------------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== restart-on-upgrade shell tests ==="
for t in ${TESTS}; do
  run_test "${t#test_}"
done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
