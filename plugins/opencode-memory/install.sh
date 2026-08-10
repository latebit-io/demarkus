#!/usr/bin/env bash
# Install the demarkus-memory OpenCode plugin from a checkout: plugin file into
# OpenCode's auto-loaded plugins dir, assets into ~/.demarkus/opencode-memory/.
# Idempotent; re-run after pulling to update; --uninstall removes.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/opencode"
PLUGIN_DEST="${OPENCODE_DIR}/plugins/demarkus-memory.ts"
SKILL_DEST="${OPENCODE_DIR}/skills/soul-memory"
ASSETS_DEST="${HOME}/.demarkus/opencode-memory"

if [[ "${1:-}" == "--uninstall" ]]; then
  rm -f "${PLUGIN_DEST}"
  rm -rf "${SKILL_DEST}" "${ASSETS_DEST}"
  echo "[demarkus-memory] uninstalled (plugin, skill, assets). ~/.demarkus state and binaries untouched."
  exit 0
fi

for d in src/demarkus-memory.ts commands context scripts skills/soul-memory/SKILL.md; do
  [[ -e "${HERE}/${d}" ]] || { echo "[demarkus-memory] install: missing ${d}; run from a full checkout" >&2; exit 1; }
done

mkdir -p "${OPENCODE_DIR}/plugins" "${SKILL_DEST}"
install -m 0644 "${HERE}/src/demarkus-memory.ts" "${PLUGIN_DEST}"
install -m 0644 "${HERE}/skills/soul-memory/SKILL.md" "${SKILL_DEST}/SKILL.md"

# Stage the full asset tree, then swap it in: an interrupted copy must not
# leave the active install with missing commands/context/scripts.
staging="${ASSETS_DEST}.staging.$$"
trap 'rm -rf "${staging}"' EXIT
rm -rf "${staging}"
mkdir -p "${staging}"
for d in commands context scripts; do
  cp -R "${HERE}/${d}" "${staging}/${d}"
done
chmod 0755 "${staging}/scripts/"*.sh
rm -rf "${ASSETS_DEST}"
mv "${staging}" "${ASSETS_DEST}"

echo "[demarkus-memory] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Start (or restart) opencode; the first session provisions the soul. Diagnose with /soul-status."
