#!/usr/bin/env bash
# Test: every session-start hook reports a failed bootstrap, provision or
# guidance step on stderr, keeps stdout clean, and still exits 0 (fails open).
#
# Each hook runs from a temp copy beside a stub bootstrap.sh and a stub binary.
#
# Usage: bash scripts/test-session-start-hooks.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# run_hook <plugin> <bootstrap exit> <failing subcommand or "none">
run_hook() {
  local plugin="$1" bootstrap_exit="$2" failing="$3"
  local tree="$TMP/$plugin.$bootstrap_exit.$failing" home="$TMP/home.$plugin.$bootstrap_exit.$failing"
  mkdir -p "$tree/hooks" "$tree/scripts" "$tree/context" "$home/.demarkus/bin"
  cp "$ROOT/plugins/$plugin/hooks/session-start.sh" "$tree/hooks/"
  : > "$tree/context/session-guidance.md"
  printf '#!/usr/bin/env bash\nexit %s\n' "$bootstrap_exit" > "$tree/scripts/bootstrap.sh"
  cat > "$home/.demarkus/bin/demarkus-plugin" <<STUB
#!/usr/bin/env bash
if [ "\$1" = "$failing" ]; then exit 7; fi
if [ "\$1" = "guidance" ]; then echo '{"ok":true}'; fi
STUB
  chmod +x "$home/.demarkus/bin/demarkus-plugin"
  HOOK_STATUS=0
  HOME="$home" bash "$tree/hooks/session-start.sh" >"$TMP/out" 2>"$TMP/err" || HOOK_STATUS=$?
}

for plugin in claude-code claude-code-knowledge cursor-memory cursor-knowledge; do
  run_hook "$plugin" 1 none
  [ "$HOOK_STATUS" = 0 ] || fail "$plugin: bootstrap failure exited $HOOK_STATUS"
  grep -q "bootstrap failed" "$TMP/err" || fail "$plugin: bootstrap failure not reported"

  run_hook "$plugin" 0 guidance
  [ "$HOOK_STATUS" = 0 ] || fail "$plugin: guidance failure exited $HOOK_STATUS"
  grep -q "guidance exited 7" "$TMP/err" || fail "$plugin: guidance failure not reported"
  [ ! -s "$TMP/out" ] || fail "$plugin: stdout not empty after a failed guidance"
done

# Only the memory plugins provision a server.
for plugin in claude-code cursor-memory; do
  run_hook "$plugin" 0 provision
  [ "$HOOK_STATUS" = 0 ] || fail "$plugin: provision failure exited $HOOK_STATUS"
  grep -q "provision exited 7" "$TMP/err" || fail "$plugin: provision failure not reported"
  grep -q '"ok":true' "$TMP/out" || fail "$plugin: guidance not emitted after a failed provision"
done

echo "PASS: session-start hooks"
