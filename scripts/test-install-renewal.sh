#!/usr/bin/env bash
# Test: certbot renewal wiring in install.sh — the deploy hook, the root cron
# entry, and the teardown that runs when a host stops using Let's Encrypt.
#
# The renewal functions are sourced out of install.sh and run against a fake
# root: crontab is stubbed over a file, the hook path is redirected into a
# tempdir. Nothing here touches the real system, so it runs anywhere.
#
# Usage: bash scripts/test-install-renewal.sh [path/to/install.sh]
#
# Runs with the same `set -euo pipefail` install.sh uses. That matters: without
# -e, an assignment from a command substitution that exits nonzero looks
# harmless here while aborting the real installer.
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/install.sh}"
[ -f "$SRC" ] || { echo "not found: $SRC" >&2; exit 1; }
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
CRON="$TMP/crontab.txt"; : > "$CRON"

# Stubs: crontab reads/writes a file, log_* are silent, chmod/mkdir are real.
# Real `crontab -` reads stdin to EOF before installing the new table, so a
# `crontab -l | ... | crontab -` pipeline does not truncate its own input.
# A stub that redirects straight to the file would fake a race that cannot
# happen in production.
# CRON_MODE models the three real outcomes of `crontab -l`:
#   ok   - table read
#   none - "no crontab for root" on stderr, exit 1 (normal on a fresh host)
#   fail - some other failure, exit 1 (must never be read as "empty")
CRON_MODE="ok"
crontab() {
  case "${1:-}" in
    -l)
      case "$CRON_MODE" in
        none) echo "no crontab for root" >&2; return 1 ;;
        fail) echo "crontab: cannot open spool" >&2; return 1 ;;
        *)    cat "$CRON" ;;
      esac
      ;;
    -)
      if [ "$CRON_MODE" = "fail" ]; then echo "crontab: install failed" >&2; return 1; fi
      local buf; buf=$(cat)
      if [ -n "$buf" ]; then printf '%s\n' "$buf" > "$CRON"; else : > "$CRON"; fi
      ;;
  esac
}
log_info() { :; }; log_warn() { :; }; log_error() { echo "ERROR: $*"; }
# Read by the functions eval'd below, which all return early off Linux.
# shellcheck disable=SC2034
PLATFORM="linux"

# Pull in only the renewal functions, redirected at the fake root.
eval "$(awk '/^CERT_DEPLOY_HOOK=/{on=1} /^setup_custom_tls\(\)/{on=0} on' "$SRC" \
  | sed "s|^CERT_DEPLOY_HOOK=.*|CERT_DEPLOY_HOOK=\"$TMP/hooks/deploy/demarkus-reload.sh\"|")"
HOOK="$TMP/hooks/deploy/demarkus-reload.sh"

pass=0; fail=0
check() { # check <desc> <condition-cmd...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "  ok   $desc"; pass=$((pass+1))
  else echo "  FAIL $desc"; fail=$((fail+1)); fi
}

echo "== fresh install (server only)"
setup_cert_renewal example.com
check "deploy hook written"            test -f "$HOOK"
check "hook is executable"             test -x "$HOOK"
check "hook HUPs the server"           grep -q "kill -HUP" "$HOOK"
check "cron line added"                grep -q "certbot renew" "$CRON"

echo "== re-run over an existing cert (the original bug)"
: > "$CRON"; rm -f "$HOOK"
setup_cert_renewal example.com
check "hook re-created on re-run"      test -f "$HOOK"
check "cron re-added on re-run"        grep -q "certbot renew" "$CRON"

echo "== library install extends the hook"
setup_library_cert_renewal
check "hook restarts the library"      grep -q "try-restart demarkus-library" "$HOOK"
check "hook still HUPs the server"     grep -q "kill -HUP" "$HOOK"
check "cron covers the library"        grep -q "try-restart demarkus-library" "$CRON"
check "exactly one cron line"          test "$(grep -c 'certbot renew' "$CRON")" -eq 1

echo "== later server re-run must not downgrade the library hook"
setup_cert_renewal example.com
check "library restart survives"       grep -q "try-restart demarkus-library" "$HOOK"

echo "== hook install leaves no staging files behind"
check "no temp files in hook dir"      test "$(find "$(dirname "$HOOK")" -name 'demarkus-reload.sh.*' | wc -l)" -eq 0

echo "== our cron entry carries an ownership marker"
check "marker on the cron line"        grep -q "demarkus-managed-renewal" "$CRON"

echo "== switching away from Let's Encrypt clears our wiring"
printf '0 3 * * * /usr/local/bin/backup.sh\n' >> "$CRON"   # unrelated user entry
# A user's own certbot job that reloads demarkus must survive: it is theirs.
printf '30 4 * * * certbot renew --deploy-hook "systemctl reload demarkus-extra"\n' >> "$CRON"
clear_cert_renewal
check "hook removed"                   test ! -f "$HOOK"
check "our cron entry removed"         bash -c "! grep -q 'demarkus-managed-renewal' '$CRON'"
check "unrelated cron entry kept"      grep -q "backup.sh" "$CRON"
check "user certbot job kept"          grep -q "demarkus-extra" "$CRON"

echo "== a legacy unmarked entry is still recognised as ours"
: > "$CRON"
printf '%s\n' '0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"' >> "$CRON"
printf '30 4 * * * certbot renew --deploy-hook "systemctl reload demarkus-extra"\n' >> "$CRON"
clear_cert_renewal
check "legacy entry removed"           bash -c "! grep -q 'pidof demarkus-server' '$CRON'"
check "user certbot job still kept"    grep -q "demarkus-extra" "$CRON"

echo "== clear is safe when nothing is installed"
clear_rc=0; clear_cert_renewal || clear_rc=$?
check "second clear reports success"   test "$clear_rc" -eq 0
check "user certbot job untouched"     grep -q "demarkus-extra" "$CRON"

echo "== our entry being the only line is not a failure"
# grep -Ev exits 1 when it filters everything out; treating that as an error
# would report a false failure, and under set -e abort the installer.
: > "$CRON"; rm -f "$HOOK"
setup_cert_renewal example.com >/dev/null
clear_rc=0; clear_cert_renewal >/dev/null || clear_rc=$?
check "clear reports success"          test "$clear_rc" -eq 0
check "table is now empty"             test ! -s "$CRON"
setup_cert_renewal example.com >/dev/null
check "re-add onto empty table works"  grep -q "demarkus-managed-renewal" "$CRON"

echo "== a failed crontab read must never cause a write"
: > "$CRON"
printf '0 3 * * * /usr/local/bin/backup.sh\n' >> "$CRON"
before=$(cat "$CRON")
CRON_MODE="fail"
clear_rc=0; clear_cert_renewal >/dev/null 2>&1 || clear_rc=$?
check "clear reports the failure"      test "$clear_rc" -ne 0
check "clear leaves the table alone"   test "$(cat "$CRON")" = "$before"
rc=0; ( setup_cert_renewal example.com >/dev/null 2>&1 ) || rc=$?
check "setup fails closed on read err" test "$rc" -ne 0
check "table survived setup failure"   test "$(cat "$CRON")" = "$before"
CRON_MODE="ok"

echo "== a host with no crontab at all still gets the entry"
: > "$CRON"; rm -f "$HOOK"
CRON_MODE="none"
# Called bare, NOT wrapped in `|| true`: a wrapper would suppress set -e inside
# the function and hide the very abort this checks for. If the installer takes
# rc=1 as fatal, this script dies here and the run reports no total at all.
setup_cert_renewal example.com >/dev/null
check "entry added on bare host"       grep -q "demarkus-managed-renewal" "$CRON"
check "no blank first line"            bash -c "! head -1 '$CRON' | grep -q '^$'"
CRON_MODE="ok"

echo "== legacy entry is migrated to the marker, not duplicated"
: > "$CRON"; rm -f "$HOOK"
printf '%s\n' '0 */12 * * * certbot renew --quiet --deploy-hook "pidof demarkus-server | xargs -r kill -HUP"' >> "$CRON"
setup_cert_renewal example.com >/dev/null
check "marker now present"             grep -q "demarkus-managed-renewal" "$CRON"
check "exactly one renewal entry"      test "$(grep -c 'certbot renew' "$CRON")" -eq 1

echo "== the user's own crontab formatting survives"
: > "$CRON"; rm -f "$HOOK"
printf '# my jobs\n\n0 3 * * * /usr/local/bin/backup.sh\n' > "$CRON"
setup_cert_renewal example.com >/dev/null
check "user comment kept"              grep -q "^# my jobs" "$CRON"
check "user blank line kept"           bash -c "sed -n '2p' '$CRON' | grep -q '^$'"
check "our entry appended"             grep -q "demarkus-managed-renewal" "$CRON"

echo "== a user job naming the library must not suppress ours"
: > "$CRON"; rm -f "$HOOK"
printf '15 2 * * * systemctl try-restart demarkus-library\n' >> "$CRON"
setup_library_cert_renewal >/dev/null
check "our library entry installed"    bash -c "grep 'demarkus-managed-renewal' '$CRON' | grep -q 'try-restart demarkus-library'"
check "user job preserved"             grep -q "^15 2 " "$CRON"

echo "== static: renewal is wired from the main flow, not from setup_tls"
# The bug was a call-graph one: setup_cert_renewal lived inside setup_tls,
# which the main flow skips whenever the certificate already exists.
in_setup_tls=$(awk '/^setup_tls\(\)/{on=1} on && /^}/{print; on=0} on' "$SRC" | grep -c "setup_cert_renewal" || true)
in_main=$(awk '/# Let.s Encrypt/{on=1} on && /^  elif/{on=0} on' "$SRC" | grep -c "setup_cert_renewal")
check "setup_tls no longer owns renewal"  test "$in_setup_tls" -eq 0
check "main LE branch calls renewal"      test "$in_main" -ge 1

# Every non-Let's-Encrypt path through the TLS block must tear our wiring down.
non_le=$(awk '/^  # TLS setup/{on=1} on && /^  # System tuning/{on=0} on' "$SRC" | grep -c "clear_cert_renewal")
check "non-LE branches clear wiring"      test "$non_le" -eq 3
# A bare call would discard the teardown result the function now returns.
guarded=$(grep -c "clear_cert_renewal || report_stale_renewal" "$SRC")
check "every call site checks result"     test "$guarded" -eq 3
check "uninstall uses remove_path"        grep -q 'remove_path "$CERT_DEPLOY_HOOK"' "$SRC"
# Pipelines must be wrapped: `check ... cmd | grep` would pipe check's own
# output into grep and lose the result.
check "uninstall cron failure tracked"    bash -c "awk '/# Remove certbot renewal cron/,/^  fi/' '$SRC' | grep -q uninstall_errors"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
