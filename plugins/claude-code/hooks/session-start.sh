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
# Finally, emit a SessionStart additionalContext payload so every session knows
# the soul exists and self-documents to it (recall via mark_lookup, record
# decisions/patterns/journal proactively). If the server isn't reachable, the
# payload leads with a warning so the agent surfaces it instead of silently
# failing memory calls. All setup logging goes to stderr; stdout carries only
# the JSON payload Claude Code parses.
#
# No core-code changes. Pure composition of existing CLI surface.

set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

# seed_souldocs — drop the bundled seed docs into an empty soul. seed_doc never
# clobbers, so this is safe to run every session: it fills in only what's
# missing (a fresh soul, or an existing one that predates a new seed doc).
seed_souldocs() {
  local seed_dir="${HOOK_DIR}/../seed"
  seed_doc "${seed_dir}/index.md"            "${SOUL_DIR}/index.md"
  seed_doc "${seed_dir}/project-template.md" "${SOUL_DIR}/project-template.md"
}

# emit_context — print the SessionStart additionalContext JSON to stdout.
# Leads with the health warning (if any) so the agent relays it, then the
# standing self-documentation guidance. Skips the guidance body gracefully if
# the file is somehow missing, still surfacing any warning.
emit_context() {
  local warning="$1"
  local payload=""
  [[ -n "${warning}" ]] && payload="⚠️ ${warning}"$'\n\n'
  if [[ -f "${GUIDANCE_FILE}" ]]; then
    payload="${payload}$(cat "${GUIDANCE_FILE}")"
  fi
  [[ -z "${payload}" ]] && return 0
  printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":%s}}\n' \
    "$(printf '%s' "${payload}" | json_escape)"
}

main() {
  local warning=""

  # Run all setup with stdout folded into stderr: stdout must carry only the
  # JSON payload emitted at the very end, regardless of what setup.sh or the
  # binary downloader print.
  {
    if ! load_config; then
      log "no plugin config — running default setup"
      "${SCRIPTS_DIR}/setup.sh" default
      load_config || die "setup.sh completed but ${PLUGIN_CONFIG} is missing or invalid"
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

    seed_souldocs

    # Best-effort health check; never abort the hook over it.
    warning="$(server_health_warning || true)"

    log "ready (mode=${MODE}, soul=${SOUL_DIR}, port=${PORT})"
  } 1>&2

  emit_context "${warning}"
}

main "$@"
