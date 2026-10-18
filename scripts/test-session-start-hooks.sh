#!/usr/bin/env bash
# Test: every session-start hook reports a failed bootstrap, provision or
# guidance step on stderr, keeps stdout clean, and still exits 0 (fails open).
#
# Each hook runs from a temp copy beside a stub bootstrap.sh and a stub binary,
# both logging to $HOOK_HOME/calls, and the plugin's real session guidance.
#
# Usage: bash scripts/test-session-start-hooks.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# run_hook <plugin> <bootstrap exit> <failing subcommand or "none"> [no-conf]
run_hook() {
  local plugin="$1" bootstrap_exit="$2" failing="$3" tree
  tree=$(mktemp -d "$TMP/hook.XXXXXX")
  HOOK_HOME="$tree/home"
  mkdir -p "$tree/hooks" "$tree/scripts" "$tree/context" "$HOOK_HOME/.demarkus/bin"
  cp "$ROOT/plugins/$plugin/hooks/session-start.sh" "$tree/hooks/"
  cp "$ROOT/plugins/$plugin/context/session-guidance.md" "$tree/context/"
  printf '#!/usr/bin/env bash\necho bootstrap >> "%s/calls"\nexit %s\n' "$HOOK_HOME" "$bootstrap_exit" > "$tree/scripts/bootstrap.sh"
  [ "${4:-}" = no-conf ] || echo "MODE=default" > "$HOOK_HOME/.demarkus/plugin-memory.conf"
  cat > "$HOOK_HOME/.demarkus/bin/demarkus-plugin" <<STUB
#!/usr/bin/env bash
echo "\$1" >> "$HOOK_HOME/calls"
if [ "\$1" = "$failing" ]; then exit 7; fi
if [ "\$1" = "guidance" ]; then echo '{"ok":true}'; fi
STUB
  chmod +x "$HOOK_HOME/.demarkus/bin/demarkus-plugin"
  HOOK_STATUS=0
  HOME="$HOOK_HOME" bash "$tree/hooks/session-start.sh" >"$TMP/out" 2>"$TMP/err" || HOOK_STATUS=$?
}

calls() { tr '\n' ' ' < "$HOOK_HOME/calls"; }

for plugin in claude-code-knowledge cursor-memory cursor-knowledge; do
  run_hook "$plugin" 1 none
  [ "$HOOK_STATUS" = 0 ] || fail "$plugin: bootstrap failure exited $HOOK_STATUS"
  grep -q "bootstrap failed" "$TMP/err" || fail "$plugin: bootstrap failure not reported"
done

for plugin in claude-code claude-code-knowledge cursor-memory cursor-knowledge; do
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

# The Claude Code memory hook leaves the download to scripts/mcp-launch.sh.
run_hook claude-code 0 none
[ "$(calls)" = "provision guidance " ] || fail "claude-code: ran $(calls)"

# No config: a provision would start a second soul; the static guidance stands
# in, without the generator's marker lines.
run_hook claude-code 0 none no-conf
[ "$HOOK_STATUS" = 0 ] || fail "claude-code: no-config run exited $HOOK_STATUS"
[ ! -e "$HOOK_HOME/calls" ] || fail "claude-code: ran $(calls)without a config"
grep -v '^<!--.*-->$' "$ROOT/plugins/claude-code/context/session-guidance.md" | diff - "$TMP/out" >/dev/null ||
  fail "claude-code: no-config output is not the static guidance"

echo "PASS: session-start hooks"
