#!/usr/bin/env bash
# promote-target.sh — manage plain remote promote targets (the registry read by
# detect-promote.sh / lib.sh promote_targets). A plain remote demarkus server
# has no broker directory to enumerate writable worlds, so its write path is
# DECLARED here at registration — the all-client-side counterpart to the
# broker's mark_worlds writable surface. No server or protocol change.
#
# Usage:
#   promote-target.sh list                      — print registered targets
#   promote-target.sh add <slug> <path> [label] — register a plain endpoint (idempotent)
#
# <slug>  the MCP server name for the endpoint (see `claude mcp list`), used as
#         mcp__<slug>__mark_publish when promoting.
# <path>  the write path or prefix on that server you may publish to (must start
#         with /). The human declares it; a denied write surfaces at publish time.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

cmd="${1:-list}"
case "${cmd}" in
  list)
    promote_targets
    ;;
  add)
    slug="${2:-}"
    path="${3:-}"
    shift 3 2>/dev/null || true
    label="${*:-}"
    [[ -n "${slug}" ]] || die "add: missing <slug> (the endpoint's MCP server name)"
    [[ "${path}" == /* ]] || die "add: <path> must start with / (got '${path:-}')"
    # The registry is space-delimited (slug path [label]); a path with internal
    # whitespace would serialize ambiguously and reparse as a truncated path.
    [[ "${path}" != *[[:space:]]* ]] || die "add: <path> must not contain whitespace (got '${path}')"
    case "${slug}" in
      *[!a-zA-Z0-9_-]*) die "add: <slug> '${slug}' has unexpected characters; use the MCP server name" ;;
    esac
    # Idempotent on slug+path: don't append a duplicate route.
    if promote_targets | awk -v s="${slug}" -v p="${path}" '$1==s && $2==p { found=1 } END { exit found?0:1 }'; then
      log "promote target ${slug} ${path} already registered"
      exit 0
    fi
    mkdir -p "${PLUGIN_HOME}"
    printf '%s %s%s\n' "${slug}" "${path}" "${label:+ ${label}}" >> "${PROMOTE_TARGETS}"
    log "registered promote target: ${slug} ${path}${label:+ (${label})}"
    ;;
  *)
    die "unknown subcommand '${cmd}' (use: list | add <slug> <path> [label])"
    ;;
esac
