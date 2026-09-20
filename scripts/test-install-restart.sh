#!/usr/bin/env bash
# Test: the updater signals only servers started from INSTALL_DIR, so a plugin
# managed server survives, and a server that is down after restart is reported.
#
# Sourced out of install.sh against real throwaway processes; no network.
#
# Usage: bash scripts/test-install-restart.sh [path/to/install.sh]
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/install.sh}"
[ -f "$SRC" ] || { echo "not found: $SRC" >&2; exit 1; }
TMP=$(mktemp -d)
PIDS=""
cleanup() {
  # shellcheck disable=SC2086  # a pid list, split on purpose
  [ -z "$PIDS" ] || kill -9 $PIDS 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

log_info() { :; }
log_warn() { :; }
log_error() { :; }

INSTALL_DIR="$TMP/bin"
# shellcheck disable=SC2034  # consumed by the functions sourced from install.sh
SUDO=""
# shellcheck disable=SC2034  # shortens the wait loops sourced from install.sh
SERVER_WAIT_SECONDS=2
mkdir -p "$INSTALL_DIR" "$TMP/plugin"

for fn in installed_server_pids stop_installed_server verify_server_running; do
  body=$(awk "/^${fn}\\(\\)/,/^\\}\$/" "$SRC")
  [ -n "$body" ] || { echo "FAIL: install.sh has no ${fn}" >&2; exit 1; }
  eval "$body"
done

fail() { echo "FAIL: $*" >&2; exit 1; }

# Both processes are named demarkus-server; only the path tells them apart.
# argv[0] is set by exec because macOS kills a copied system binary.
( exec -a "$INSTALL_DIR/demarkus-server" sleep 60 ) &
installed=$!
( exec -a "$TMP/plugin/demarkus-server" sleep 60 ) &
plugin=$!
PIDS="$installed $plugin"
# Keeps the shell from reporting the kills in cleanup.
disown "$installed" "$plugin"
sleep 1

listed=$(installed_server_pids)
[ "$listed" = "$installed" ] || fail "installed_server_pids = '$listed', want '$installed'"

# shellcheck disable=SC2034  # read by verify_server_running
PLATFORM="darwin"
verify_server_running || fail "verify_server_running: want success while the server runs"

stop_installed_server
kill -0 "$installed" 2>/dev/null && fail "installed server still running after stop"
kill -0 "$plugin" 2>/dev/null || fail "plugin managed server was killed"

verify_server_running && fail "verify_server_running: want failure with the server down"

# Linux trusts the unit state, not the process table.
# shellcheck disable=SC2034  # read by verify_server_running
PLATFORM="linux"
systemctl() { [ "$1" = "is-active" ] && return 3; return 0; }
verify_server_running && fail "verify_server_running: want failure for an inactive unit"
systemctl() { return 0; }
verify_server_running || fail "verify_server_running: want success for an active unit"

echo "PASS: install restart"
