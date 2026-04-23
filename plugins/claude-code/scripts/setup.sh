#!/usr/bin/env bash
# setup.sh — configure the demarkus-memory plugin's soul.
#
# Usage:
#   setup.sh default                       # try port 6310 at ~/.demarkus/soul/;
#                                          # falls back to isolated if 6310 is taken
#   setup.sh reuse --port N --root PATH    # adopt an already-running server
#   setup.sh isolated                      # always spawn ours at ~/.demarkus/plugin-soul/
#                                          # on a free port in the isolated range
#
# Writes ~/.demarkus/plugin-memory.conf on success. Idempotent.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

do_default() {
  ensure_binaries
  if ! port_is_free "${DEFAULT_PORT}"; then
    log "port ${DEFAULT_PORT} is in use — falling back to isolated mode"
    do_isolated
    return
  fi
  ensure_token_entry "${SHARED_SOUL_DIR}/tokens.toml"
  ensure_managed_server "${SHARED_SOUL_DIR}" "${DEFAULT_PORT}"
  save_config "${SHARED_SOUL_DIR}" "${DEFAULT_PORT}" "default"
  log "default setup complete (soul=${SHARED_SOUL_DIR}, port=${DEFAULT_PORT})"
}

do_isolated() {
  ensure_binaries
  local port
  port=$(find_free_port_from "${ISOLATED_PORT_START}")
  ensure_token_entry "${ISOLATED_SOUL_DIR}/tokens.toml"
  ensure_managed_server "${ISOLATED_SOUL_DIR}" "${port}"
  save_config "${ISOLATED_SOUL_DIR}" "${port}" "isolated"
  log "isolated setup complete (soul=${ISOLATED_SOUL_DIR}, port=${port})"
}

do_reuse() {
  local port="" root=""
  while (( $# )); do
    case "$1" in
      --port) port="$2"; shift 2 ;;
      --root) root="$2"; shift 2 ;;
      *) die "unknown arg: $1" ;;
    esac
  done
  [[ -n "${port}" && -n "${root}" ]] \
    || die "reuse requires --port N --root PATH"

  ensure_binaries
  ensure_token_entry "${root}/tokens.toml"

  # Ask the adopted server to reload tokens. It may not be ours, but SIGHUP
  # is the documented mechanism and has no effect on other signal handlers.
  local target_pid
  target_pid=$(pgrep -f "demarkus-server.*-root +${root}\b" 2>/dev/null | head -1 || true)
  if [[ -n "${target_pid}" ]]; then
    if kill -HUP "${target_pid}" 2>/dev/null; then
      log "sent SIGHUP to adopted server (pid=${target_pid}) to reload tokens"
    else
      warn "SIGHUP to pid ${target_pid} failed; tokens may not be active until the server restarts"
    fi
  else
    warn "no running server found with -root ${root}; proceeding with config, but /soul-init may need a rerun once it's up"
  fi

  save_config "${root}" "${port}" "reuse"
  log "reuse setup complete (soul=${root}, port=${port})"
}

main() {
  local mode="${1:-}"
  shift || true
  case "${mode}" in
    default)  do_default ;;
    reuse)    do_reuse "$@" ;;
    isolated) do_isolated ;;
    *)        die "usage: setup.sh default | reuse --port N --root PATH | isolated" ;;
  esac
}

main "$@"
