#!/usr/bin/env bash
# Test: every Claude Code hook command in the plugin manifests starts under a
# plugin root containing a space. Claude Code runs hook commands through a
# shell, so an unquoted ${CLAUDE_PLUGIN_ROOT} splits and the gate fails open.
#
# Usage: bash scripts/test-hook-commands.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

HOME_STUB="$TMP/home"
mkdir -p "$HOME_STUB/.demarkus/bin"
# Every subcommand succeeds silently, so a hook that starts exits 0 fast.
printf '#!/usr/bin/env bash\nexit 0\n' > "$HOME_STUB/.demarkus/bin/demarkus-plugin"
chmod +x "$HOME_STUB/.demarkus/bin/demarkus-plugin"

checked=0
for plugin in claude-code claude-code-knowledge; do
  manifest="$ROOT/plugins/$plugin/.claude-plugin/plugin.json"
  root="$TMP/plug in root/$plugin"
  mkdir -p "$(dirname "$root")"
  cp -R "$ROOT/plugins/$plugin" "$root"
  printf '#!/usr/bin/env bash\nexit 0\n' > "$root/scripts/bootstrap.sh"

  while IFS= read -r cmd; do
    [ -n "$cmd" ] || continue
    case "$cmd" in
      '"${CLAUDE_PLUGIN_ROOT}"/hooks/'*.sh) ;;
      *) fail "$plugin: hook command does not quote the plugin root: $cmd" ;;
    esac
    err="$TMP/err"
    if ! CLAUDE_PLUGIN_ROOT="$root" HOME="$HOME_STUB" bash -c "$cmd" </dev/null >"$TMP/out" 2>"$err"; then
      fail "$plugin: hook command failed under a spaced root: $cmd ($(cat "$err"))"
    fi
    grep -qi 'no such file\|command not found' "$err" && fail "$plugin: hook did not start under a spaced root: $cmd ($(cat "$err"))"
    checked=$((checked + 1))
  done < <(sed -n 's/^[[:space:]]*"command": "\(.*\)",\{0,1\}$/\1/p' "$manifest" | sed 's/\\"/"/g')
done

[ "$checked" -ge 10 ] || fail "expected at least 10 hook commands, checked $checked"
echo "✓ $checked hook commands start under a spaced plugin root"
