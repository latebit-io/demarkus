#!/usr/bin/env bash
# /knowledge-join script half. Validates a demarkus-broker URL and emits
# the values the slash command needs to wire `claude mcp add`. The
# command-side prompt parses our key=value output; do not change the
# output shape without updating commands/knowledge-join.md.
#
# Usage: knowledge-join.sh <broker-url>
# Output (stdout, on success):
#   OK
#   url=<canonicalized broker URL, no trailing slash>
#   slug=<lowercase-sanitized first DNS label of the host>
#   mcp-url=<url>/mcp
# Output (stdout, on failure):
#   FAIL: <one-line operator-readable message>
# Exit codes: 0 on success, non-zero on validation/network failure. The
# slash command treats any non-OK output as "do not run claude mcp add."

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
# shellcheck source=./lib.sh
. "${SCRIPT_DIR}/lib.sh"

# fail emits the operator-readable error on stdout (so the slash command
# can show it verbatim) AND on stderr (so a human running the script
# directly sees it), then exits non-zero. Two surfaces, one message.
fail() {
  local msg="$1"
  echo "FAIL: ${msg}"
  echo "[demarkus-memory] error: ${msg}" >&2
  exit 1
}

if [[ $# -ne 1 ]]; then
  fail "usage: knowledge-join.sh <broker-url>"
fi

URL="$1"

# Scheme validation. https:// is required for any real deployment;
# the test harness flips DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP=1 to drive
# a local http:// mock without round-tripping through TLS plumbing. The
# env var is deliberately long + namespaced so accidental shell exports
# don't enable it.
#
# Per RFC 3986, scheme matching is case-insensitive — `HTTPS://...` and
# `Https://...` are valid. Bash 3.2 (macOS stock) lacks `${URL,,}`
# lower-casing, so use bracketed character classes for each scheme byte
# instead. Surgical (only these two patterns get the treatment) rather
# than `shopt -s nocasematch` which would silently affect every later
# case statement in the script.
case "${URL}" in
  [Hh][Tt][Tt][Pp][Ss]://*) ;;
  [Hh][Tt][Tt][Pp]://*)
    if [[ "${DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP:-}" != "1" ]]; then
      fail "broker URL must use https:// (got: ${URL})"
    fi
    ;;
  *)
    fail "broker URL must start with https:// (got: ${URL})"
    ;;
esac

# Strip trailing slashes so the joined /mcp URL is well-formed regardless
# of how the user pasted the URL.
URL="${URL%/}"

# Normalize the scheme portion to lowercase. Case-insensitive scheme
# validation above accepts `HTTPS://`, but everything downstream
# (`claude mcp list`, broker logs, the emitted url=... line) reads
# cleaner with `https://`. The scheme is guaranteed to be present at
# this point — the case-validation block rejected anything else.
URL="$(printf '%s' "${URL%%://*}" | tr '[:upper:]' '[:lower:]')://${URL#*://}"

command -v curl >/dev/null 2>&1 || fail "curl is required"

# RFC 9728 OAuth-protected-resource metadata is the natural validation
# surface — Slice 7 of the gateway plan stood it up specifically as the
# "is this a Slice-7+ broker" signal. A HEAD is enough; we don't need
# the body, just the status code. --max-time bounds the worst-case wait
# on a hung TLS handshake; --connect-timeout bounds DNS + TCP setup.
META_URL="${URL}/.well-known/oauth-protected-resource"
# curl writes "000" to stdout via -w on connection-level failure (DNS,
# TCP refused, TLS handshake error) AND returns a non-zero exit. Using
# `|| echo "000"` would concatenate "000" + "000" → "000000" on the
# failure path. Suppress the OR and fall back via parameter expansion
# for the rare case where curl produced no output at all (e.g. SIGKILL).
http_code=$(curl -sS -o /dev/null -w "%{http_code}" \
              --max-time 15 --connect-timeout 5 \
              -I "${META_URL}" 2>/dev/null) || true
http_code="${http_code:-000}"

case "${http_code}" in
  200)
    : # validated
    ;;
  000)
    fail "broker unreachable at ${META_URL} (DNS / TCP / TLS failure — check the URL and network)"
    ;;
  401|403)
    fail "broker rejected metadata fetch with HTTP ${http_code} (metadata endpoint should be public — confirm this is a Slice 7+ demarkus-broker)"
    ;;
  404)
    fail "broker has no /.well-known/oauth-protected-resource (HTTP 404) — this is not a Slice 7+ demarkus-broker, or the URL points at the management API instead of the MCP host"
    ;;
  *)
    fail "broker metadata fetch returned HTTP ${http_code} (expected 200)"
    ;;
esac

# Derive the slug from the first DNS label of the host. Examples:
#   https://mcp.broker.acme.com    → acme
#   https://broker.acme.com        → broker  (note: the FIRST label, not "acme")
#   https://acme-broker.example    → acme-broker
# The "first label" rule is naive but matches typical enterprise URL
# shapes (broker is the first-label subdomain of the org). If the
# heuristic produces a wrong slug for a deployment, the user can rename
# the MCP server entry via `claude mcp` after the fact — we are not
# trying to be clever here.
# Strip everything up to and including `://` so the scheme drops out
# regardless of case. Cheaper than two case-insensitive prefix patterns
# and the case-validation step above already guaranteed a scheme is
# present, so the # match here is always non-empty.
HOST="${URL#*://}"
HOST="${HOST%%/*}"
HOST="${HOST%%:*}"          # strip :port if present
SLUG_RAW="${HOST%%.*}"      # first DNS label

# Sanitize: lowercase, replace non-[a-z0-9-] with -, collapse + trim
# leading/trailing hyphens. The output is what shows up as the MCP
# server name in `claude mcp list`, so it must satisfy whatever
# claude-cli accepts (a conservative [a-z0-9-]+ keeps us safe).
SLUG=$(printf '%s' "${SLUG_RAW}" \
       | tr '[:upper:]' '[:lower:]' \
       | tr -c 'a-z0-9-' '-' \
       | tr -s '-' \
       | sed 's/^-*//;s/-*$//')

if [[ -z "${SLUG}" ]]; then
  fail "could not derive a usable MCP server slug from URL ${URL} (host: ${HOST}) — rename manually with \`claude mcp\` after a fallback install"
fi

echo "OK"
echo "url=${URL}"
echo "slug=${SLUG}"
echo "mcp-url=${URL}/mcp"
