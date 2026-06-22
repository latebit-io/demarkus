#!/usr/bin/env bash
# /soul-default script half. Lists the joined-soul catalog for selection and
# records the chosen soul as THIS project's default write target — the binding in
# ~/.demarkus/project-souls that the destination gate enforces. Unlike
# /soul-join, it joins nothing and touches no MCP config or token: it only
# re-points an already-joined repo at a different default. The command-side
# prompt parses the line-oriented output; do not change the output shape without
# updating commands/soul-default.md.
#
# Usage:
#   soul-default.sh --list --bind DIR        # enumerate the catalog for selection
#   soul-default.sh --set SLUG --bind DIR    # save SLUG as DIR's default target
#
# --list output (stdout):
#   CATALOG                       (header) — or EMPTY when nothing is joined
#   <id>\t<tier>\t<host>\t<insecure>\t<current>   (one row per soul)
# where <current> is "*" for DIR's existing default, else "-".
# --set output (stdout): "OK <slug>" on success, "FAIL: <message>" on failure.
# Exit 0 on success; non-zero on validation failure. Reads/echoes no token.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
# shellcheck source=./lib.sh
. "${SCRIPT_DIR}/lib.sh"

# fail emits the operator-readable error on stdout (so the slash command shows it
# verbatim) AND on stderr (so a human running the script directly sees it).
fail() {
  local msg="$1"
  echo "FAIL: ${msg}"
  echo "[demarkus-memory] error: ${msg}" >&2
  exit 1
}

MODE=""
SLUG=""
BIND_DIR=""

while (( $# )); do
  case "$1" in
    --list) [[ -z "${MODE}" ]] || fail "choose one of --list / --set"; MODE="list"; shift ;;
    --set)  [[ -z "${MODE}" ]] || fail "choose one of --list / --set"
            [[ -n "${2:-}" ]] || fail "--set requires a soul slug"; MODE="set"; SLUG="$2"; shift 2 ;;
    --bind) [[ -n "${2:-}" ]] || fail "--bind requires a directory"; BIND_DIR="$2"; shift 2 ;;
    -*)     fail "unknown option: $1" ;;
    *)      fail "unexpected argument: $1" ;;
  esac
done

[[ -n "${MODE}" ]]     || fail "usage: soul-default.sh (--list | --set SLUG) --bind DIR"
[[ -n "${BIND_DIR}" ]] || fail "--bind DIR is required (the project directory to set the default for)"

current="$(project_soul_binding "${BIND_DIR}")"

case "${MODE}" in
  list)
    catalog="$(soul_catalog)"
    if [[ -z "${catalog}" ]]; then
      echo "EMPTY"
      exit 0
    fi
    echo "CATALOG"
    # Append the current-default marker column so the command can pre-tick the row.
    printf '%s\n' "${catalog}" | awk -F '\t' -v cur="${current}" \
      '{ print $0 "\t" ($1 == cur ? "*" : "-") }'
    ;;
  set)
    is_catalog_soul "${SLUG}" \
      || fail "'${SLUG}' is not a joined soul — run /soul-join first (see /soul-default --list for the catalog)"
    bind_project_soul "${BIND_DIR}" "${SLUG}" \
      || fail "could not write the binding for ${BIND_DIR}"
    echo "OK ${SLUG}"
    ;;
esac
