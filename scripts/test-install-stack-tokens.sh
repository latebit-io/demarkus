#!/usr/bin/env bash
# Test: install-stack.sh token rotation — token_valid must accept only the
# recursive /** mint shape (legacy /* tokens rotate), and ensure_token must
# reuse a valid cached token, rotate a stale one (revoke before mint), and
# fail closed when the store is unreadable.
#
# Functions are sourced out of install-stack.sh and run against a fake
# INSTALL_DIR carrying a stub demarkus-token. Nothing touches the real system.
#
# Usage: bash scripts/test-install-stack-tokens.sh [path/to/install-stack.sh]
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/install-stack.sh}"
[ -f "$SRC" ] || { echo "not found: $SRC" >&2; exit 1; }
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

log_info() { :; }
log_warn() { :; }
log_error() { :; }

# Extract the functions under test from the installer.
eval "$(awk '/^token_valid\(\)/,/^\}$/' "$SRC")"
eval "$(awk '/^ensure_token\(\)/,/^\}$/' "$SRC")"

INSTALL_DIR="$TMP/bin"
SERVER_TOKENS="$TMP/tokens.toml"
# Read the mint policy from the installer itself, then pin it: a regression of
# TOKEN_PATHS back to "/*" must fail here, not silently satisfy the fixtures.
# shellcheck disable=SC2034 # consumed by the eval'd ensure_token
TOKEN_PATHS="$(sed -n 's/^TOKEN_PATHS="\([^"]*\)"$/\1/p' "$SRC")"
# shellcheck disable=SC2034 # consumed by the eval'd ensure_token
TOKEN_OPS="$(sed -n 's/^TOKEN_OPS="\([^"]*\)"$/\1/p' "$SRC")"
if [ "$TOKEN_PATHS" != "/**" ] || [ "$TOKEN_OPS" != "publish" ]; then
  echo "unexpected installer token policy: paths=$TOKEN_PATHS ops=$TOKEN_OPS" >&2
  exit 1
fi
mkdir -p "$INSTALL_DIR"

# Stub demarkus-token mirroring the real behavior ensure_token relies on:
# list decodes (exit 0 on a readable store), revoke drops the label's stanza
# (fixtures hold one stanza, so truncate), generate appends an entry whose
# hash matches the raw it prints (so token_valid accepts the mint).
cat > "$INSTALL_DIR/demarkus-token" <<'EOF'
#!/usr/bin/env bash
cmd="$1"; shift
label=""; paths=""; tokens=""
while [ $# -gt 0 ]; do
  case "$1" in
    -label) label="$2"; shift 2 ;;
    -paths) paths="$2"; shift 2 ;;
    -tokens) tokens="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case "$cmd" in
  list)   [ -r "$tokens" ] || exit 1 ;;
  revoke)
    [ -n "${FAKE_REVOKE_FAIL:-}" ] && exit 1
    echo "revoke $label" >> "${STUB_CALLS:?}"; : > "$tokens" ;;
  generate)
    echo "generate $label $paths" >> "${STUB_CALLS:?}"
    raw="minted-raw-token"
    sum=$(printf %s "$raw" | sha256sum | cut -d' ' -f1)
    printf '[tokens.%s]\nhash = "sha256-%s"\npaths = ["%s"]\noperations = ["%s"]\n' \
      "$label" "$sum" "$paths" "publish" >> "$tokens"
    echo "$raw"
    ;;
esac
EOF
chmod +x "$INSTALL_DIR/demarkus-token"
export STUB_CALLS="$TMP/calls.txt"
: > "$STUB_CALLS"

hash_of() { printf 'sha256-%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }

write_entry() { # $1 raw token, $2 paths list (TOML fragment)
  cat > "$SERVER_TOKENS" <<EOF
[tokens.test]
hash = "$(hash_of "$1")"
paths = [$2]
operations = ["publish"]
EOF
}

fails=0
check_rc() { # $1 description, $2 expected rc, $3 raw token
  local rc=0
  token_valid "$3" || rc=$?
  if [ "$rc" -ne "$2" ]; then
    echo "FAIL: $1 (rc=$rc, want $2)" >&2
    fails=$((fails + 1))
  else
    echo "ok: $1"
  fi
}

# --- token_valid scope gate ---------------------------------------------------
write_entry raw1 '"/**"'
check_rc "recursive /** scope is valid" 0 raw1

write_entry raw2 '"/*"'
check_rc "legacy /* scope rotates" 1 raw2

write_entry raw3 '"/*", "/**"'
check_rc "manually widened /*,/** is valid" 0 raw3

write_entry raw4 '"/**"'
check_rc "unknown raw token rotates" 1 other-raw

rm -f "$SERVER_TOKENS"
check_rc "missing store is a read error" 2 raw1

# --- ensure_token -------------------------------------------------------------
# Valid cached token: reused verbatim, no revoke/generate.
write_entry cached-raw '"/**"'
: > "$STUB_CALLS"
out=$(ensure_token test "test" cached-raw)
if [ "$out" = "cached-raw" ] && [ ! -s "$STUB_CALLS" ]; then
  echo "ok: valid cached token is reused without minting"
else
  echo "FAIL: reuse path (out=$out, calls=$(cat "$STUB_CALLS"))" >&2
  fails=$((fails + 1))
fi

# Legacy-scope cached token: rotated — revoke then generate /**, new raw out.
write_entry cached-raw '"/*"'
: > "$STUB_CALLS"
out=$(ensure_token test "test" cached-raw)
if [ "$out" = "minted-raw-token" ] \
  && grep -q '^revoke test$' "$STUB_CALLS" \
  && grep -q '^generate test /\*\*$' "$STUB_CALLS"; then
  echo "ok: legacy-scope token rotates via revoke + /** mint"
else
  echo "FAIL: rotation path (out=$out, calls=$(cat "$STUB_CALLS"))" >&2
  fails=$((fails + 1))
fi

# Unreadable store with a cached token: fail closed, no mint.
rm -f "$SERVER_TOKENS"
: > "$STUB_CALLS"
rc=0
out=$(ensure_token test "test" cached-raw) || rc=$?
if [ "$rc" -ne 0 ] && [ ! -s "$STUB_CALLS" ]; then
  echo "ok: unreadable store fails closed without minting"
else
  echo "FAIL: unreadable-store path (rc=$rc, calls=$(cat "$STUB_CALLS"))" >&2
  fails=$((fails + 1))
fi

# Revoke failure fails closed: no mint attempted against an unverified store.
write_entry cached-raw '"/*"'
: > "$STUB_CALLS"
rc=0
out=$(FAKE_REVOKE_FAIL=1 ensure_token test "test" cached-raw) || rc=$?
if [ "$rc" -ne 0 ] && ! grep -q '^generate' "$STUB_CALLS"; then
  echo "ok: revoke failure fails closed without minting"
else
  echo "FAIL: revoke-failure path (rc=$rc, calls=$(cat "$STUB_CALLS"))" >&2
  fails=$((fails + 1))
fi

# No cached token: idempotent revoke first, then mint.
write_entry raw9 '"/**"'
: > "$STUB_CALLS"
out=$(ensure_token test "test" "")
if [ "$out" = "minted-raw-token" ] && grep -q '^generate test /\*\*$' "$STUB_CALLS"; then
  echo "ok: fresh mint uses the /** scope"
else
  echo "FAIL: fresh-mint path (out=$out, calls=$(cat "$STUB_CALLS"))" >&2
  fails=$((fails + 1))
fi

if [ "$fails" -gt 0 ]; then
  echo "$fails failure(s)" >&2
  exit 1
fi
echo "all checks passed"
