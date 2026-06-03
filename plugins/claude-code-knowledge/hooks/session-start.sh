#!/usr/bin/env bash
# SessionStart hook for the demarkus-knowledge plugin.
#
# This plugin manages no server and downloads no binaries — a knowledge system is
# reached over the broker via `claude mcp add` + Claude Code's MCP OAuth. So the
# hook does one thing: emit a SessionStart additionalContext payload.
#
#   - If one or more knowledge systems are joined (registered by /knowledge-join),
#     name them and inject the standing guidance: consult the system first for
#     shared/org subjects, navigate via the root hub, record to the shared
#     catalog, and (when a local soul also exists) keep the soul↔system lanes.
#   - If none are joined yet, emit a single one-time pointer to /knowledge-join,
#     then stay quiet on subsequent sessions so an unused install isn't noisy.
#
# stdout carries ONLY the JSON payload Claude Code parses; everything else goes
# to stderr. Fails open: any hiccup means no context, never a wedged session.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
GUIDANCE_FILE="${HOOK_DIR}/../context/session-guidance.md"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

HINT_SENTINEL="${PLUGIN_HOME}/.knowledge-join-hint-shown"

# emit — print the SessionStart additionalContext JSON for the given body.
emit() {
  local payload="$1"
  [[ -z "${payload}" ]] && return 0
  printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":%s}}\n' \
    "$(printf '%s' "${payload}" | json_escape)"
}

main() {
  local systems
  systems="$(list_knowledge_systems)"

  if [[ -z "${systems}" ]]; then
    # Not joined to anything yet. Point at the join command exactly once, then
    # stay silent so a plugin installed "for later" doesn't nag every session.
    if [[ ! -e "${HINT_SENTINEL}" ]]; then
      mkdir -p "${PLUGIN_HOME}" 2>/dev/null || true
      : > "${HINT_SENTINEL}" 2>/dev/null || true
      emit "demarkus-knowledge is installed but no knowledge system is joined yet. To connect this Claude Code installation to your organization's shared demarkus knowledge base, run \`/knowledge-join <broker-url>\`."
    fi
    return 0
  fi

  # One or more systems joined — build the standing guidance.
  local header="# demarkus knowledge system(s) joined"$'\n\n'
  header="${header}You are connected to the following organizational demarkus knowledge system(s), available as MCP servers (use \`mark://<world>/<path>\` URLs against them):"$'\n'
  local slug
  while IFS= read -r slug; do
    [[ -z "${slug}" ]] && continue
    header="${header}"$'\n'"- **${slug}** (MCP server \`${slug}\`)"
  done <<< "${systems}"
  header="${header}"$'\n'

  local body=""
  [[ -f "${GUIDANCE_FILE}" ]] && body="$(cat "${GUIDANCE_FILE}")"

  local soul_note=""
  if ! soul_is_configured; then
    # No local soul → drop the soul-specific lane note so the guidance doesn't
    # reference a store the user doesn't have.
    soul_note=$'\n\n'"(No local demarkus soul is configured here, so the soul↔system guidance above is informational only — everything durable goes to the knowledge system.)"
  fi

  emit "${header}"$'\n'"${body}${soul_note}"
}

main "$@"
