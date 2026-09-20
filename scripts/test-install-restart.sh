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

for fn in server_exe_path resolved_path installed_server_pids stop_installed_server verify_server_running; do
  body=$(awk "/^${fn}\\(\\)/,/^\\}\$/" "$SRC")
  [ -n "$body" ] || { echo "FAIL: install.sh has no ${fn}" >&2; exit 1; }
  eval "$body"
done

fail() { echo "FAIL: $*" >&2; exit 1; }

# Three servers: the installed one by full path, the installed one through
# PATH (bare argv[0]), and a plugin managed one that must never be touched.
mkdir -p "$TMP/plugin"
if [ "$(uname -s)" = "Linux" ]; then
  cp "$(command -v sleep)" "$INSTALL_DIR/demarkus-server"
  cp "$(command -v sleep)" "$TMP/plugin/demarkus-server"
  "$INSTALL_DIR/demarkus-server" 60 &
  installed=$!
  PATH="$INSTALL_DIR:$PATH" demarkus-server 60 &
  by_path=$!
  "$TMP/plugin/demarkus-server" 60 &
  plugin=$!
  # An update replaces the binary under a running server; /proc then reports
  # the path with " (deleted)" and the link dangles. It must still be found.
  sleep 1
  rm "$INSTALL_DIR/demarkus-server"
else
  # macOS kills a copied system binary, so argv[0] is faked with exec and the
  # executable lookup is stubbed to say what a real copy would report.
  ( exec -a "$INSTALL_DIR/demarkus-server" sleep 60 ) &
  installed=$!
  ( exec -a demarkus-server sleep 60 ) &
  by_path=$!
  ( exec -a "$TMP/plugin/demarkus-server" sleep 60 ) &
  plugin=$!
  # Only this test's own pids are claimed; any real server keeps its real path.
  server_exe_path() {
    case "$1" in
      "$installed"|"$by_path") echo "$INSTALL_DIR/demarkus-server" ;;
      "$plugin") echo "$TMP/plugin/demarkus-server" ;;
      *) ps -o comm= -p "$1" 2>/dev/null || true ;;
    esac
  }
fi
PIDS="$installed $by_path $plugin"
# Keeps the shell from reporting the kills in cleanup.
disown "$installed" "$by_path" "$plugin"
sleep 1

listed=$(installed_server_pids | sort -n | tr '\n' ' ')
want=$(printf '%s\n%s\n' "$installed" "$by_path" | sort -n | tr '\n' ' ')
[ "$listed" = "$want" ] || fail "installed_server_pids = '$listed', want '$want'"

# shellcheck disable=SC2034  # read by verify_server_running
PLATFORM="darwin"
verify_server_running || fail "verify_server_running: want success while the server runs"

stop_installed_server
kill -0 "$installed" 2>/dev/null && fail "installed server still running after stop"
kill -0 "$by_path" 2>/dev/null && fail "PATH started installed server still running after stop"
kill -0 "$plugin" 2>/dev/null || fail "plugin managed server was killed"

verify_server_running && fail "verify_server_running: want failure with the server down"

# A live candidate that cannot be inspected aborts instead of being skipped:
# skipping could replace the binary under a running server.
real_exe_path=$(declare -f server_exe_path)
server_exe_path() { if [ "$1" = "$plugin" ]; then echo ""; else ps -o comm= -p "$1" 2>/dev/null || true; fi; }
installed_server_pids >/dev/null && fail "installed_server_pids: want failure for an uninspectable live process"
stop_installed_server && fail "stop_installed_server: want failure when inspection fails"
kill -0 "$plugin" 2>/dev/null || fail "uninspectable process was killed"
eval "$real_exe_path"

# Linux trusts the unit state, not the process table.
# shellcheck disable=SC2034  # read by verify_server_running
PLATFORM="linux"
systemctl() { [ "$1" = "is-active" ] && return 3; return 0; }
verify_server_running && fail "verify_server_running: want failure for an inactive unit"
systemctl() { return 0; }
verify_server_running || fail "verify_server_running: want success for an active unit"

echo "PASS: install restart"
