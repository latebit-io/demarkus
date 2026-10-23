#!/usr/bin/env bash
# Test: a macOS uninstall removes only the installer's files from the shared
# ~/.demarkus, so the agent plugins' binaries, memory and tokens survive.
#
# Sourced out of install.sh against a throwaway HOME; no network, no launchd.
#
# Usage: bash scripts/test-install-uninstall.sh [path/to/install.sh]
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/install.sh}"
[ -f "$SRC" ] || { echo "not found: $SRC" >&2; exit 1; }
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

for fn in installer_config_entries plist_content_root do_uninstall; do
  body=$(awk "/^${fn}\\(\\)/,/^\\}\$/" "$SRC")
  [ -n "$body" ] || { echo "FAIL: install.sh has no ${fn}" >&2; exit 1; }
  eval "$body"
done

log_info() { :; }
log_warn() { :; }
log_error() { :; }
log_step() { :; }
check_install_permissions() { :; }
launchctl() { :; }
sw_vers() { echo 15.0; }
# shellcheck disable=SC2034  # consumed by the functions sourced from install.sh
detect_platform() {
  PLATFORM=darwin
  CONFIG_DIR="$HOME/.demarkus"
}
# shellcheck disable=SC2034  # consumed by the functions sourced from install.sh
SUDO=""
# shellcheck disable=SC2034
LIBRARY_CONFIG_DIR="$TMP/etc/demarkus-library"

fail() { echo "FAIL: $*" >&2; exit 1; }

# write_install lays down what install.sh writes on macOS for a content root.
write_install() {
  local root="$1"
  mkdir -p "$INSTALL_DIR" "$HOME/Library/LaunchAgents" "$HOME/.demarkus/tls" \
    "$HOME/.demarkus/logs" "$root"
  for bin in demarkus-server demarkus demarkus-install; do
    touch "$INSTALL_DIR/$bin"
  done
  touch "$HOME/.demarkus/version" "$HOME/.demarkus/initial-token.txt" \
    "$HOME/.demarkus/tokens.toml" "$HOME/.demarkus/tls/cert.pem" \
    "$HOME/.demarkus/logs/demarkus.log" "$HOME/.demarkus/logs/demarkus.err" \
    "$root/index.md"
  cat > "$HOME/Library/LaunchAgents/io.latebit.demarkus.plist" <<EOF
<dict>
    <key>DEMARKUS_ROOT</key>
    <string>${root}</string>
    <key>DEMARKUS_TOKENS</key>
    <string>$HOME/.demarkus/tokens.toml</string>
</dict>
EOF
}

# Case 1: installer and memory plugin share ~/.demarkus.
HOME="$TMP/shared"
INSTALL_DIR="$TMP/shared-bin"
write_install "$HOME/.demarkus/content"
[ "$(plist_content_root "$HOME/Library/LaunchAgents/io.latebit.demarkus.plist")" = "$HOME/.demarkus/content" ] \
  || fail "plist_content_root did not read DEMARKUS_ROOT"
plugin_state="bin/demarkus-plugin soul/index.md tokens/soul-1a2b3c4d/tokens.toml logs/server-soul-1a2b3c4d.log plugin-memory.conf project-souls"
for p in $plugin_state; do
  mkdir -p "$(dirname "$HOME/.demarkus/$p")"
  touch "$HOME/.demarkus/$p"
done
( do_uninstall --keep-data ) || fail "uninstall exited non-zero"
for p in $plugin_state; do
  [ -e "$HOME/.demarkus/$p" ] || fail "plugin state removed: ~/.demarkus/$p"
done
for p in version initial-token.txt tokens.toml tls logs/demarkus.log logs/demarkus.err; do
  [ ! -e "$HOME/.demarkus/$p" ] || fail "installer file left: ~/.demarkus/$p"
done
[ -e "$HOME/.demarkus/content/index.md" ] || fail "--keep-data removed the content"
[ ! -e "$HOME/Library/LaunchAgents/io.latebit.demarkus.plist" ] || fail "plist left"
[ ! -e "$INSTALL_DIR/demarkus-server" ] || fail "binary left"

# Case 2: installer only, content elsewhere; the emptied ~/.demarkus goes.
HOME="$TMP/alone"
INSTALL_DIR="$TMP/alone-bin"
write_install "$TMP/site"
( do_uninstall --keep-data ) || fail "uninstall exited non-zero"
[ ! -e "$HOME/.demarkus" ] || fail "empty ~/.demarkus left: $(ls -A "$HOME/.demarkus")"
[ -e "$TMP/site/index.md" ] || fail "--keep-data removed the content"

echo "PASS: macOS uninstall keeps plugin state and removes only installer files"
