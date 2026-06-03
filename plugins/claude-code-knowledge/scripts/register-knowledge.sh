#!/usr/bin/env bash
# Record a joined knowledge-system MCP server slug so the publish tag-gate
# enforces on writes to it (same as the local soul). Idempotent. Invoked by the
# /knowledge-join command after `claude mcp add` succeeds.
#
# Usage: register-knowledge.sh <slug>

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

[[ $# -eq 1 && -n "${1:-}" ]] || die "usage: register-knowledge.sh <slug>"

register_knowledge_system "$1"
log "registered knowledge system '${1}' for publish-gate enforcement"
