#!/usr/bin/env bash
# Test: both installers refuse to install an archive they could not verify.
# Regression for skip paths that warned and installed anyway.
#
# Usage: bash scripts/test-install-checksum.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

echo payload > "$TMP/a.tar.gz"
good=$(sha "$TMP/a.tar.gz")

for installer in install.sh install-readonly.sh; do
  # Subshell per case: verify_sha256 exits the shell on refusal.
  run() {
    (
      log_info() { :; }
      log_warn() { :; }
      log_error() { :; }
      eval "$(awk '/^verify_sha256\(\)/,/^\}$/' "$ROOT/$installer")"
      verify_sha256 "$@"
    ) >/dev/null 2>&1
  }

  echo "$good  a.tar.gz" > "$TMP/sums"
  run "$TMP/a.tar.gz" "$TMP/sums" || fail "$installer: matching checksum must pass"

  echo "$good  a.tar.gz.sbom" > "$TMP/sums"
  ! run "$TMP/a.tar.gz" "$TMP/sums" || fail "$installer: entry for another file must not verify"

  echo "0000  a.tar.gz" > "$TMP/sums"
  ! run "$TMP/a.tar.gz" "$TMP/sums" || fail "$installer: mismatch must fail"

  : > "$TMP/sums"
  ! run "$TMP/a.tar.gz" "$TMP/sums" || fail "$installer: missing entry must fail"

  ! run "$TMP/a.tar.gz" "$TMP/absent" || fail "$installer: missing checksums file must fail"

  ! grep -n "skipping verification\|verification will be skipped" "$ROOT/$installer" \
    || fail "$installer: still has a verification skip path"
done

echo "PASS"
