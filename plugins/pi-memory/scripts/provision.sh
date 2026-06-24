#!/usr/bin/env bash
# provision.sh — per-session server provisioning for the demarkus-memory pi
# extension. This is the setup half of the claude-code plugin's session-start.sh,
# lifted verbatim minus the SessionStart context emission (the pi extension's
# TypeScript builds and injects that). It manages the SAME ~/.demarkus state as
# the claude-code plugin, so the two interoperate: one soul, one token.
#
# Everything goes to stderr; stdout is intentionally empty so a caller can run
# this purely for its side effects.

set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
SEED_DIR="${SCRIPTS_DIR}/../seed"
# shellcheck source=lib.sh
. "${SCRIPTS_DIR}/lib.sh"

seed_souldocs() {
  seed_doc "${SEED_DIR}/index.md"            "${SOUL_DIR}/index.md"
  seed_doc "${SEED_DIR}/project-template.md" "${SOUL_DIR}/project-template.md"
}

main() {
  {
    if ! load_config; then
      log "no plugin config — running default setup"
      "${SCRIPTS_DIR}/setup.sh" default
      load_config || die "setup.sh completed but ${PLUGIN_CONFIG} is missing or invalid"
    fi

    ensure_binaries
    restart_local_server_on_upgrade

    case "${MODE}" in
      default|isolated)
        ensure_managed_server "${SOUL_DIR}" "${PORT}"
        ;;
      reuse)
        if [[ -z "$(pid_of_server_at_root "${SOUL_DIR}")" ]]; then
          warn "configured to reuse server at ${SOUL_DIR} but none is running; run /soul-init to reconfigure"
        fi
        ;;
      *)
        die "unknown MODE in ${PLUGIN_CONFIG}: ${MODE}"
        ;;
    esac

    seed_souldocs
    log "ready (mode=${MODE}, soul=${SOUL_DIR}, port=${PORT})"
  } 1>&2
}

main "$@"
