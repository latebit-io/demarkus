#!/usr/bin/env bash
# SessionStart hook for the demarkus-memory plugin.
#
# On every session:
#   - If the plugin is already configured (~/.demarkus/plugin-memory.conf exists),
#     ensure the managed server is running and seed /index.md if the soul is empty.
#   - If not configured yet, run the default setup automatically: try port 6310 at
#     ~/.demarkus/soul/, fall back to an isolated install on 16310+ if 6310 is
#     taken. The user can rerun /soul-init at any time to reconfigure
#     (e.g. to adopt an existing demarkus-server they were already running).
#
# No core-code changes. Pure composition of existing CLI surface.

set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

seed_index() {
  local seed_file="${HOOK_DIR}/../seed/index.md"
  local target="${SOUL_DIR}/index.md"
  [[ -d "${SOUL_DIR}" && -w "${SOUL_DIR}" ]] || return 0
  [[ -f "${target}"    ]] && return 0
  [[ -f "${seed_file}" ]] || return 0
  cp "${seed_file}" "${target}"
  log "seeded ${target}"
}

main() {
  if ! load_config; then
    log "no plugin config — running default setup"
    "${SCRIPTS_DIR}/setup.sh" default
    load_config
  fi

  ensure_binaries

  case "${MODE}" in
    default|isolated)
      ensure_managed_server "${SOUL_DIR}" "${PORT}"
      ;;
    reuse)
      # User-managed server; verify it's still up but don't try to start it.
      if [[ -z "$(pid_of_server_at_root "${SOUL_DIR}")" ]]; then
        warn "configured to reuse server at ${SOUL_DIR} but none is running; run /soul-init to reconfigure"
      fi
      ;;
    *)
      die "unknown MODE in ${PLUGIN_CONFIG}: ${MODE}"
      ;;
  esac

  seed_index

  log "ready (mode=${MODE}, soul=${SOUL_DIR}, port=${PORT})"
}

main "$@"
