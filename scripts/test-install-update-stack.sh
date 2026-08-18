#!/usr/bin/env bash
# Test: install.sh update path — update_stack_component must refresh the
# optional single-host components (broker, library) in place where they are
# installed, skip them silently where they are not, and never touch config or
# systemd units. Regression for a stack host whose library was installed once
# and then never refreshed by `demarkus-install update`.
#
# Functions are sourced out of install.sh and run against a fake INSTALL_DIR
# and SYSTEMD_DIR. Nothing touches the real system and nothing is downloaded.
#
# Usage: bash scripts/test-install-update-stack.sh [path/to/install.sh]
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/install.sh}"
[ -f "$SRC" ] || { echo "not found: $SRC" >&2; exit 1; }
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

log_info() { :; }
log_warn() { :; }
log_error() { :; }
log_step() { :; }

INSTALL_DIR="$TMP/bin"
SYSTEMD_DIR="$TMP/systemd"
PLATFORM="linux"
SUDO=""
mkdir -p "$INSTALL_DIR" "$SYSTEMD_DIR"

# Record what the updater would have done instead of doing it.
CALLS="$TMP/calls"
: > "$CALLS"
fetch_library_binary() { echo "fetch_library $*" >> "$CALLS"; _LIBRARY_VERSION="9.9.9"; }
download_and_verify_asset() { echo "download $1 $2" >> "$CALLS"; }
install_binaries() { echo "install $2" >> "$CALLS"; }
# Stub systemctl so a restart is recorded, never executed.
systemctl() { echo "systemctl $*" >> "$CALLS"; }

eval "$(awk '/^update_stack_component\(\)/,/^\}$/' "$SRC")"

fail() { echo "FAIL: $1" >&2; exit 1; }

# 1. Absent component: no download, no restart.
: > "$CALLS"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
[ ! -s "$CALLS" ] || fail "an uninstalled component must be skipped silently, got: $(cat "$CALLS")"

# 2. Installed via binary: refreshed and restarted.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-library"; chmod +x "$INSTALL_DIR/demarkus-library"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
grep -q "^fetch_library " "$CALLS" || fail "installed library was not refreshed"
grep -q "try-restart demarkus-library" "$CALLS" || fail "library was not restarted"

# 3. Installed via unit only (binary elsewhere): still refreshed.
: > "$CALLS"
rm -f "$INSTALL_DIR/demarkus-library"
touch "$SYSTEMD_DIR/demarkus-library.service"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
grep -q "^fetch_library " "$CALLS" || fail "unit-only install was not refreshed"

# 4. Broker takes its binary from the tools release.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-broker"; chmod +x "$INSTALL_DIR/demarkus-broker"
update_stack_component "demarkus-broker" "demarkus-broker" "0.15.3" "$TMP"
grep -q "^download demarkus-broker 0.15.3" "$CALLS" || fail "broker not downloaded from tools"
grep -q "^install demarkus-broker" "$CALLS" || fail "broker binary not installed"
grep -q "try-restart demarkus-broker" "$CALLS" || fail "broker was not restarted"

# 5. Broker with no resolved tools release: skipped, not half-updated.
: > "$CALLS"
update_stack_component "demarkus-broker" "demarkus-broker" "" "$TMP"
grep -q "^download" "$CALLS" && fail "broker updated without a tools version"
grep -q "try-restart" "$CALLS" && fail "broker restarted without being updated"

# 6. Non-linux hosts have no stack services.
: > "$CALLS"
PLATFORM="darwin"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
[ ! -s "$CALLS" ] || fail "stack update must be linux-only, got: $(cat "$CALLS")"

echo "PASS: update_stack_component"
