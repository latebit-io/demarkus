#!/usr/bin/env bash
# install.sh — install the demarkus-memory OpenCode plugin from a checkout.
# OpenCode auto-loads plugin files from ~/.config/opencode/plugins/, so no npm
# publish is needed: this copies the plugin file there and the assets it reads
# (commands, context, skills, scripts) to ~/.demarkus/opencode-memory/.
# Idempotent; re-run after pulling to update. Remove with --uninstall.

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

mkdir -p "${OPENCODE_DIR}/plugins" "${SKILL_DEST}" "${ASSETS_DEST}"
install -m 0644 "${HERE}/src/demarkus-memory.ts" "${PLUGIN_DEST}"
install -m 0644 "${HERE}/skills/soul-memory/SKILL.md" "${SKILL_DEST}/SKILL.md"
# rsync-free copy: replace asset dirs wholesale so deletions propagate.
for d in commands context scripts; do
  rm -rf "${ASSETS_DEST}/${d}"
  cp -R "${HERE}/${d}" "${ASSETS_DEST}/${d}"
done
chmod 0755 "${ASSETS_DEST}/scripts/"*.sh

echo "[demarkus-memory] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Start (or restart) opencode; the first session provisions the soul. Diagnose with /soul-status."
