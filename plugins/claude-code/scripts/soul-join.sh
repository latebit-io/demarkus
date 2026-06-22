#!/usr/bin/env bash
# /soul-join script half. Validates a remote demarkus (direct-QUIC) soul URL,
# derives an MCP server slug, stores the auth token in a 0600 file (NOT inline in
# the MCP config), installs the self-contained launch wrapper to a stable path,
# and records the soul in the catalog. The command-side prompt parses our
# key=value output and runs `claude mcp add` against the wrapper; do not change
# the output shape without updating commands/soul-join.md.
#
# Usage: soul-join.sh <mark://host[:port] | host[:port]> [--token T] [--insecure] [--bind DIR]
# Output (stdout, on success):
#   OK
#   slug=<sanitized first DNS label>
#   host=<normalized mark:// host>
#   insecure=<0|1>
#   token-file=<path|->
#   wrapper=<absolute path to the installed launch wrapper>
# Output (stdout, on failure):
#   FAIL: <one-line operator-readable message>
# Exit codes: 0 on success, non-zero on validation failure. The slash command
# treats any non-OK output as "do not run claude mcp add."
#
# Reachability is NOT probed here: demarkus is QUIC (no HTTP .well-known to curl
# like the broker), and the first MCP tool call is the real validation. The
# command doc says so explicitly.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
# shellcheck source=./lib.sh
. "${SCRIPT_DIR}/lib.sh"

# fail emits the operator-readable error on stdout (so the slash command can show
# it verbatim) AND on stderr (so a human running the script directly sees it).
fail() {
  local msg="$1"
  echo "FAIL: ${msg}"
  echo "[demarkus-memory] error: ${msg}" >&2
  exit 1
}

HOST_ARG=""
TOKEN=""
INSECURE=0
BIND_DIR=""

while (( $# )); do
  case "$1" in
    --token)    [[ -n "${2:-}" ]] || fail "--token requires a value"; TOKEN="$2"; shift 2 ;;
    --insecure) INSECURE=1; shift ;;
    --bind)     [[ -n "${2:-}" ]] || fail "--bind requires a value"; BIND_DIR="$2"; shift 2 ;;
    -* )        fail "unknown option: $1" ;;
    *)          [[ -z "${HOST_ARG}" ]] || fail "unexpected extra argument: $1"; HOST_ARG="$1"; shift ;;
  esac
done

[[ -n "${HOST_ARG}" ]] || fail "usage: soul-join.sh <mark://host[:port] | host[:port]> [--token T] [--insecure] [--bind DIR]"

# Scheme handling. A remote soul is direct-QUIC (mark://). Accept a bare host
# (scheme inferred) or an explicit mark:// URL; reject http(s):// outright —
# that is a broker/knowledge endpoint and belongs to /knowledge-join, not here.
# Scheme matching is case-insensitive per RFC 3986; bracketed classes work on
# Bash 3.2 (macOS) which lacks ${var,,}.
case "${HOST_ARG}" in
  [Mm][Aa][Rr][Kk]://*) HOST="mark://${HOST_ARG#*://}" ;;
  [Hh][Tt][Tt][Pp][Ss]://* | [Hh][Tt][Tt][Pp]://*)
    fail "remote souls are direct-QUIC (mark://). For an https:// broker-fronted knowledge system use /knowledge-join (got: ${HOST_ARG})" ;;
  *://*)
    fail "unsupported scheme — a remote soul URL is mark://host[:port] or a bare host (got: ${HOST_ARG})" ;;
  *) HOST="mark://${HOST_ARG}" ;;
esac

# Strip a trailing slash / path so -host is well-formed; demarkus-mcp wants just
# mark://host[:port].
HOST="${HOST%%/}"
HOSTPORT="${HOST#mark://}"
HOSTPORT="${HOSTPORT%%/*}"
[[ -n "${HOSTPORT}" ]] || fail "could not parse a host from ${HOST_ARG}"
HOST="mark://${HOSTPORT}"

# Derive the slug from the first DNS label of the host (port stripped). Same
# rule + sanitization as the knowledge plugin's join: lowercase, non-[a-z0-9-]
# → '-', collapse, trim. The slug is the MCP server name (mcp__<slug>__mark_*).
HOSTNAME_ONLY="${HOSTPORT%%:*}"
SLUG_RAW="${HOSTNAME_ONLY%%.*}"
SLUG=$(printf '%s' "${SLUG_RAW}" \
       | tr '[:upper:]' '[:lower:]' \
       | tr -c 'a-z0-9-' '-' \
       | tr -s '-' \
       | sed 's/^-*//;s/-*$//')

[[ -n "${SLUG}" ]] || fail "could not derive a usable MCP server slug from ${HOST_ARG} (host: ${HOSTNAME_ONLY})"

# "demarkus-memory" is the local managed soul's reserved server name; a remote
# soul must not shadow it (the publish gate and tool routing key on the name).
[[ "${SLUG}" == "demarkus-memory" ]] && \
  fail "slug 'demarkus-memory' is reserved for the local managed soul — re-run with an explicit different name once renaming is supported, or join a host with a different first label"

# Ensure demarkus-mcp (the wrapper's exec target) is installed + pinned. The
# test harness sets DEMARKUS_SOUL_JOIN_SKIP_BINARIES=1 to exercise validation +
# registration offline without the network download. The env var is deliberately
# long + namespaced so an accidental shell export can't silently skip it.
if [[ "${DEMARKUS_SOUL_JOIN_SKIP_BINARIES:-}" != "1" ]]; then
  ensure_binaries || fail "could not install demarkus binaries (see log above)"
  # If that upgrade swapped the SERVER binary too, the running local managed soul
  # would otherwise keep serving the stale binary until the next session start.
  # Restart it now from its own config.
  restart_local_server_on_upgrade
fi

# Install the self-contained launch wrapper to a STABLE, version-independent path
# under ${PLUGIN_BIN_DIR}. claude mcp add records an absolute command path; if we
# pointed it at the versioned plugin-cache copy, a plugin upgrade would move the
# script out from under the registered MCP server. The wrapper sources nothing
# and reads the catalog directly, so the stable copy never goes stale.
WRAPPER_SRC="${SCRIPT_DIR}/soul-remote-wrapper.sh"
WRAPPER_DST="${PLUGIN_BIN_DIR}/soul-remote-wrapper.sh"
[[ -f "${WRAPPER_SRC}" ]] || fail "launch wrapper missing at ${WRAPPER_SRC}"
mkdir -p "${PLUGIN_BIN_DIR}"
install -m 0755 "${WRAPPER_SRC}" "${WRAPPER_DST}" \
  || fail "could not install launch wrapper to ${WRAPPER_DST}"

# Token → 0600 file, never inline. "-" means no auth (a read-only / public soul).
TOKEN_FILE="-"
if [[ -n "${TOKEN}" ]]; then
  TOKEN_FILE="${PLUGIN_HOME}/soul-${SLUG}.token"
  mkdir -p "${PLUGIN_HOME}"
  printf '%s' "${TOKEN}" > "${TOKEN_FILE}.tmp"
  chmod 600 "${TOKEN_FILE}.tmp"
  mv "${TOKEN_FILE}.tmp" "${TOKEN_FILE}"
  log "stored auth token for '${SLUG}' at ${TOKEN_FILE} (mode 600)"
fi

register_remote_soul "${SLUG}" "${HOST}" "${INSECURE}" "${TOKEN_FILE}" \
  || fail "could not record soul '${SLUG}' in the catalog (${SOULS_REGISTRY})"
log "registered remote soul '${SLUG}' (host=${HOST}, insecure=${INSECURE})"

# Bind this repo to the soul when invoked inside a project (project scope).
if [[ -n "${BIND_DIR}" ]]; then
  bind_project_soul "${BIND_DIR}" "${SLUG}" \
    || fail "could not bind project ${BIND_DIR} to soul '${SLUG}'"
  log "bound project ${BIND_DIR} → soul '${SLUG}'"
fi

echo "OK"
echo "slug=${SLUG}"
echo "host=${HOST}"
echo "insecure=${INSECURE}"
echo "token-file=${TOKEN_FILE}"
echo "wrapper=${WRAPPER_DST}"
