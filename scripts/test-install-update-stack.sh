#!/usr/bin/env bash
# Test: update_stack_component refreshes an installed broker/library in place,
# skips an absent one, and never touches config or units. Regression for a
# stack host whose library was installed once and never refreshed by `update`.
#
# Sourced out of install.sh against a fake INSTALL_DIR/SYSTEMD_DIR; no network.
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
# shellcheck disable=SC2034  # consumed by the function sourced from install.sh
PLATFORM="linux"
# shellcheck disable=SC2034  # consumed by the function sourced from install.sh
SUDO=""
mkdir -p "$INSTALL_DIR" "$SYSTEMD_DIR"

# Record what the updater would have done instead of doing it.
CALLS="$TMP/calls"
: > "$CALLS"
fetch_library_binary() { echo "fetch_library $*" >> "$CALLS"; _LIBRARY_VERSION="9.9.9"; }
download_and_verify_asset() { echo "download $1 $2" >> "$CALLS"; }
install_binary_atomic() { echo "install $(basename "$2")" >> "$CALLS"; }
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

# 6. An explicit pin is honoured instead of resolving latest.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-library"; chmod +x "$INSTALL_DIR/demarkus-library"
update_stack_component "demarkus-library" "demarkus-library" "0.23.2" "$TMP"
grep -q "^fetch_library 0.23.2 " "$CALLS" || fail "pinned library version not passed through: $(cat "$CALLS")"
rm -f "$INSTALL_DIR/demarkus-library"

# 7. Non-linux hosts have no stack services.
: > "$CALLS"
# shellcheck disable=SC2034  # consumed by the function sourced from install.sh
PLATFORM="darwin"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
[ ! -s "$CALLS" ] || fail "stack update must be linux-only, got: $(cat "$CALLS")"

# 8. install_binary_atomic must replace by rename, not write in place: writing
# over a live executable fails with ETXTBSY. A rename gives dest a new inode;
# an in-place copy keeps the old one, so the inode is the observable proof.
eval "$(awk '/^install_binary_atomic\(\)/,/^\}$/' "$SRC")"
inode_of() { ls -i "$1" | awk '{print $1}'; }
DEST="$TMP/bin/demarkus-fake"
printf 'old\n' > "$DEST"; chmod 755 "$DEST"
BEFORE=$(inode_of "$DEST")
printf 'new\n' > "$TMP/new-binary"
install_binary_atomic "$TMP/new-binary" "$DEST" || fail "atomic replace failed"
AFTER=$(inode_of "$DEST")
[ "$BEFORE" != "$AFTER" ] || fail "dest was written in place (inode unchanged); a live executable would give ETXTBSY"
grep -q "^new$" "$DEST" || fail "new content not in place"
[ -x "$DEST" ] || fail "replacement is not executable"
[ -z "$(find "$TMP/bin" -name 'demarkus-fake.new.*' 2>/dev/null)" ] || fail "staging file left behind"

echo "PASS: update_stack_component"
