#!/usr/bin/env bash
# soul-remote-wrapper.sh — launches demarkus-mcp for a joined REMOTE soul.
#
# Claude Code launches MCP subprocesses with a fixed env, so the token can't be
# handed over at spawn time. .mcp.json invokes this wrapper (via `claude mcp add
# <slug> -- bash <this> <slug>`); it looks the slug up in the soul catalog,
# exports DEMARKUS_AUTH from the slug's 0600 token file, and execs demarkus-mcp
# pointed at the remote host. Keeping the token here (env, from a 0600 file)
# rather than inline in the MCP config is the whole point of /soul-join over
# hand-wiring.
#
# Self-contained ON PURPOSE: /soul-join installs this to the stable path
# ~/.demarkus/bin/soul-remote-wrapper.sh and registers THAT path. It sources no
# lib.sh and reads the catalog directly, so a plugin upgrade (which moves the
# versioned plugin-cache copy) never strands the registered MCP server.
#
# Usage: soul-remote-wrapper.sh <slug>

set -euo pipefail

SLUG="${1:-}"
if [[ -z "${SLUG}" ]]; then
  echo "[demarkus-memory] soul-remote-wrapper: missing slug argument" >&2
  exit 1
fi
# Consume the slug so it isn't forwarded to demarkus-mcp as a stray positional;
# the trailing "$@" then carries only any args Claude Code appends at launch.
shift

PLUGIN_HOME="${HOME}/.demarkus"
SOULS_REGISTRY="${PLUGIN_HOME}/souls"
MCP_BIN="${PLUGIN_HOME}/bin/demarkus-mcp"

if [[ ! -f "${SOULS_REGISTRY}" ]]; then
  echo "[demarkus-memory] no soul catalog at ${SOULS_REGISTRY} — re-run /soul-join" >&2
  exit 1
fi

# Pull the row for this slug: tab-separated "<slug>\t<host>\t<insecure>\t<token-file>".
ROW="$(awk -F '\t' -v s="${SLUG}" 'NF && $1 !~ /^#/ && $1 == s { print $2 "\t" $3 "\t" $4; exit }' \
        "${SOULS_REGISTRY}" 2>/dev/null || true)"
if [[ -z "${ROW}" ]]; then
  echo "[demarkus-memory] soul '${SLUG}' not in the catalog — re-run /soul-join" >&2
  exit 1
fi

IFS=$'\t' read -r HOST INSECURE TOKEN_FILE <<<"${ROW}"
if [[ -z "${HOST}" ]]; then
  echo "[demarkus-memory] catalog row for '${SLUG}' has no host — re-run /soul-join" >&2
  exit 1
fi

if [[ ! -x "${MCP_BIN}" ]]; then
  echo "[demarkus-memory] demarkus-mcp not installed at ${MCP_BIN} — run /soul-init or /soul-join" >&2
  exit 1
fi

# Auth: a "-" token-file means a no-auth (read-only/public) soul. Otherwise the
# file must exist and be non-empty; a present-but-empty file is a setup error
# worth surfacing rather than silently launching unauthenticated.
if [[ -n "${TOKEN_FILE}" && "${TOKEN_FILE}" != "-" ]]; then
  if [[ ! -f "${TOKEN_FILE}" ]]; then
    echo "[demarkus-memory] token file ${TOKEN_FILE} for '${SLUG}' is missing — re-run /soul-join --token" >&2
    exit 1
  fi
  DEMARKUS_AUTH="$(cat "${TOKEN_FILE}")"
  if [[ -z "${DEMARKUS_AUTH}" ]]; then
    echo "[demarkus-memory] token file ${TOKEN_FILE} for '${SLUG}' is empty — re-run /soul-join --token" >&2
    exit 1
  fi
  export DEMARKUS_AUTH
fi

ARGS=(-host "${HOST}")
[[ "${INSECURE}" == "1" ]] && ARGS+=(-insecure)

exec "${MCP_BIN}" "${ARGS[@]}" "$@"
