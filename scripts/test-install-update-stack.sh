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
# shellcheck disable=SC2034  # read by the functions sourced from install.sh
PLATFORM="linux"
# shellcheck disable=SC2034  # consumed by the function sourced from install.sh
SUDO=""
mkdir -p "$INSTALL_DIR" "$SYSTEMD_DIR"

# Record what the updater would have done instead of doing it.
CALLS="$TMP/calls"
: > "$CALLS"
fetch_library_binary() { echo "fetch_library $*" >> "$CALLS"; _LIBRARY_VERSION="9.9.9"; }
download_and_verify_asset() { echo "download $1 $2 $3" >> "$CALLS"; }
download_asset_file() { echo "download_file $1 $2" >> "$CALLS"; }
install_binary_atomic() { echo "install $(basename "$2")" >> "$CALLS"; }
# Stub systemctl so a restart is recorded, never executed.
systemctl() { echo "systemctl $*" >> "$CALLS"; }

eval "$(awk '/^stack_component_installed\(\)/,/^\}$/' "$SRC")"
eval "$(awk '/^update_stack_components\(\)/,/^\}$/' "$SRC")"
eval "$(awk '/^update_stack_component\(\)/,/^\}$/' "$SRC")"
eval "$(awk '/^migrate_broker_single_host\(\)/,/^\}$/' "$SRC")"

# Migration env: fake dirs, recorded user/ownership mutations. `id` reports
# only the legacy user so the rename branch runs.
BROKER_SERVICE="demarkus-knowledge-broker"
# shellcheck disable=SC2034  # read by the function sourced from install.sh
BROKER_LEGACY_SERVICE="demarkus-broker"
BROKER_CONFIG_DIR="$TMP/etc/demarkus-knowledge-broker"
BROKER_LEGACY_CONFIG_DIR="$TMP/etc/demarkus-broker"
BROKER_STATE_DIR="$TMP/lib/demarkus-knowledge-broker"
BROKER_LEGACY_STATE_DIR="$TMP/lib/demarkus-broker"
id() { [ "$1" = "demarkus-broker" ]; }
usermod() { echo "usermod $*" >> "$CALLS"; }
groupmod() { echo "groupmod $*" >> "$CALLS"; }
chown() { echo "chown $*" >> "$CALLS"; }

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

# 4. Broker takes its binary (and checksums) from the broker release stream.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-knowledge-broker"; chmod +x "$INSTALL_DIR/demarkus-knowledge-broker"
update_stack_component "demarkus-knowledge-broker" "demarkus-knowledge-broker" "0.15.3" "$TMP"
grep -q "^download demarkus-knowledge-broker 0.15.3 broker" "$CALLS" || fail "broker not downloaded from the broker stream"
grep -q "^download_file broker/v0.15.3 demarkus-broker_checksums.txt" "$CALLS" || fail "broker checksums not fetched from the broker stream"
grep -q "^install demarkus-knowledge-broker" "$CALLS" || fail "broker binary not installed"
grep -q "try-restart demarkus-knowledge-broker" "$CALLS" || fail "broker was not restarted"

# 5. Broker with no resolved tools release: skipped, not half-updated.
: > "$CALLS"
update_stack_component "demarkus-knowledge-broker" "demarkus-knowledge-broker" "" "$TMP"
grep -q "^download" "$CALLS" && fail "broker updated without a tools version"
grep -q "try-restart" "$CALLS" && fail "broker restarted without being updated"

# 6. An explicit pin is honoured instead of resolving latest.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-library"; chmod +x "$INSTALL_DIR/demarkus-library"
update_stack_component "demarkus-library" "demarkus-library" "0.23.2" "$TMP"
grep -q "^fetch_library 0.23.2 " "$CALLS" || fail "pinned library version not passed through: $(cat "$CALLS")"
rm -f "$INSTALL_DIR/demarkus-library"

# 7. Default (empty) version reaches the resolver rather than being skipped.
: > "$CALLS"
touch "$INSTALL_DIR/demarkus-library"; chmod +x "$INSTALL_DIR/demarkus-library"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
grep -q "^fetch_library  " "$CALLS" || grep -q "^fetch_library $" "$CALLS" \
  || grep -qE "^fetch_library( |$)" "$CALLS" || fail "empty version did not reach the resolver: $(cat "$CALLS")"

# 8. A failed restart is recorded, not swallowed.
: > "$CALLS"
_STACK_RESTART_FAILED=0
systemctl() { echo "systemctl $*" >> "$CALLS"; return 1; }
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
[ "${_STACK_RESTART_FAILED:-0}" = "1" ] || fail "a failed restart must be recorded"
systemctl() { echo "systemctl $*" >> "$CALLS"; }
_STACK_RESTART_FAILED=0
rm -f "$INSTALL_DIR/demarkus-library"

# 9. Non-linux hosts have no stack services.
: > "$CALLS"
PLATFORM="darwin"
update_stack_component "demarkus-library" "demarkus-library" "" "$TMP"
[ ! -s "$CALLS" ] || fail "stack update must be linux-only, got: $(cat "$CALLS")"
PLATFORM="linux"

# 10. An unresolvable tools release must fail loudly when a broker is installed,
# not log a skip and report success.
# shellcheck disable=SC2034  # consumed by the functions sourced from install.sh
BROKER_SERVICE="demarkus-knowledge-broker"
# shellcheck disable=SC2034  # consumed by the functions sourced from install.sh
LIBRARY_SERVICE="demarkus-library"
touch "$INSTALL_DIR/demarkus-knowledge-broker"; chmod +x "$INSTALL_DIR/demarkus-knowledge-broker"
if ( update_stack_components "" "" "$TMP" ) 2>/dev/null; then
  fail "an installed broker with no tools release must not report success"
fi
rm -f "$INSTALL_DIR/demarkus-knowledge-broker"
# With no broker installed the same call is a no-op, not a failure.
( update_stack_components "" "" "$TMP" ) 2>/dev/null || fail "absent broker must not fail the update"

# 11. A non-linux host skips the whole refresh, even with a stray binary and no
# resolvable tools release: the platform check must precede the broker guard.
PLATFORM="darwin"
touch "$INSTALL_DIR/demarkus-knowledge-broker"; chmod +x "$INSTALL_DIR/demarkus-knowledge-broker"
: > "$CALLS"
( update_stack_components "" "" "$TMP" ) 2>/dev/null || fail "non-linux refresh must not fail the update"
[ ! -s "$CALLS" ] || fail "non-linux host attempted a component refresh: $(cat "$CALLS")"
rm -f "$INSTALL_DIR/demarkus-knowledge-broker"
# shellcheck disable=SC2034  # read by the functions sourced from install.sh
PLATFORM="linux"

# 12. install_binary_atomic must replace by rename, not write in place: writing
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

# 13. Pre-rename broker install migrates in place: unit, config, state,
# binary, user all renamed, references rewritten, service re-enabled.
: > "$CALLS"
mkdir -p "$BROKER_LEGACY_CONFIG_DIR" "$BROKER_LEGACY_STATE_DIR"
printf 'storage:\n  backend: file\n  dir: %s\n' "/var/lib/demarkus-broker" > "$BROKER_LEGACY_CONFIG_DIR/config.yaml"
printf 'refresh\n' > "$BROKER_LEGACY_STATE_DIR/refresh_tokens.json"
printf '[Service]\nUser=demarkus-broker\nExecStart=%s/demarkus-broker -config /etc/demarkus-broker/config.yaml\n' "$INSTALL_DIR" > "$SYSTEMD_DIR/demarkus-broker.service"
printf 'oldbin\n' > "$INSTALL_DIR/demarkus-broker"; chmod +x "$INSTALL_DIR/demarkus-broker"
migrate_broker_single_host
[ -f "$SYSTEMD_DIR/demarkus-knowledge-broker.service" ] || fail "unit not renamed"
[ ! -e "$SYSTEMD_DIR/demarkus-broker.service" ] || fail "legacy unit left behind"
grep -q "User=demarkus-knowledge-broker" "$SYSTEMD_DIR/demarkus-knowledge-broker.service" || fail "unit User not rewritten"
grep -q "demarkus-knowledge-knowledge" "$SYSTEMD_DIR/demarkus-knowledge-broker.service" && fail "unit rewrite double-replaced"
[ -f "$BROKER_CONFIG_DIR/config.yaml" ] || fail "config dir not moved"
grep -q "dir: /var/lib/demarkus-knowledge-broker" "$BROKER_CONFIG_DIR/config.yaml" || fail "config storage.dir not rewritten"
[ -f "$BROKER_STATE_DIR/refresh_tokens.json" ] || fail "state dir not moved"
[ -x "$INSTALL_DIR/demarkus-knowledge-broker" ] || fail "binary not renamed"
[ ! -e "$INSTALL_DIR/demarkus-broker" ] || fail "legacy binary left behind"
grep -q "^usermod -l demarkus-knowledge-broker demarkus-broker" "$CALLS" || fail "user not renamed"
grep -q "systemctl enable demarkus-knowledge-broker" "$CALLS" || fail "migrated service not re-enabled"
grep -q "systemctl restart demarkus-knowledge-broker" "$CALLS" || fail "migrated service not restarted"

# 14. Migration is one-shot: a second run must be a silent no-op.
: > "$CALLS"
migrate_broker_single_host
[ ! -s "$CALLS" ] || fail "migration reran on a migrated install: $(cat "$CALLS")"
grep -q "demarkus-knowledge-knowledge" "$BROKER_CONFIG_DIR/config.yaml" && fail "config double-replaced on rerun"
rm -f "$INSTALL_DIR/demarkus-knowledge-broker"

echo "PASS: update_stack_component"
