#!/usr/bin/env bash
# Install the OpenCode knowledge adapter, native skill, and runtime assets.
# Supports a checkout or curl-piped standalone install.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/opencode"
PLUGIN_DEST="${OPENCODE_DIR}/plugins/demarkus-knowledge.ts"
SKILL_DEST="${OPENCODE_DIR}/skills/knowledge-promote"
ASSETS_DEST="${HOME}/.demarkus/opencode-knowledge"
MEMORY_PLUGIN="${OPENCODE_DIR}/plugins/demarkus-memory.ts"

if [[ "${1:-}" == "--uninstall" ]]; then
  rm -f "${PLUGIN_DEST}"
  rm -rf "${SKILL_DEST}" "${ASSETS_DEST}"
  echo "[demarkus-knowledge] uninstalled plugin, skill, and assets. Shared registry and OAuth state remain."
  exit 0
fi

if [[ -e "${MEMORY_PLUGIN}" ]] && ! grep -q 'io.demarkus.opencode.gate-adapters' "${MEMORY_PLUGIN}"; then
  echo "[demarkus-knowledge] install: update demarkus-opencode-memory first; the installed adapter cannot coordinate publish gates" >&2
  echo "  curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash" >&2
  exit 1
fi

tmpdir=""
staging=""
previous_assets=""
cleanup() {
  if [[ -n "${previous_assets}" && -e "${previous_assets}" && ! -e "${ASSETS_DEST}" ]]; then
    mv "${previous_assets}" "${ASSETS_DEST}" \
      || echo "[demarkus-knowledge] install: asset restore failed; backup preserved at ${previous_assets}" >&2
  fi
  rm -rf "${tmpdir}" "${staging}"
}
trap cleanup EXIT

SRC="${HERE}"
if [[ ! -e "${SRC}/src/demarkus-knowledge.ts" ]]; then
  REF="${DEMARKUS_REF:-main}"
  command -v curl >/dev/null 2>&1 || { echo "[demarkus-knowledge] install: curl required" >&2; exit 1; }
  command -v tar >/dev/null 2>&1 || { echo "[demarkus-knowledge] install: tar required" >&2; exit 1; }
  tmpdir="$(mktemp -d)"
  topdir="demarkus-${REF//\//-}"
  echo "[demarkus-knowledge] downloading plugin (ref ${REF})..." >&2
  curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 \
    "https://codeload.github.com/latebit-io/demarkus/tar.gz/${REF}" \
    | tar -xz -C "${tmpdir}" "${topdir}/plugins/opencode-knowledge" \
    || { echo "[demarkus-knowledge] install: download/extract failed for ref ${REF}" >&2; exit 1; }
  SRC="${tmpdir}/${topdir}/plugins/opencode-knowledge"
fi

for path in src/demarkus-knowledge.ts package.json commands context scripts skills/knowledge-promote/SKILL.md; do
  [[ -e "${SRC}/${path}" ]] || { echo "[demarkus-knowledge] install: missing ${path} in ${SRC}" >&2; exit 1; }
done

staging="${ASSETS_DEST}.staging.$$"
rm -rf "${staging}"
mkdir -p "${staging}/assets"
for path in commands context scripts; do
  cp -R "${SRC}/${path}" "${staging}/assets/${path}"
done
chmod 0755 "${staging}/assets/scripts/"*.sh
install -m 0644 "${SRC}/package.json" "${staging}/assets/package.json"
install -m 0644 "${SRC}/src/demarkus-knowledge.ts" "${staging}/demarkus-knowledge.ts"
install -m 0644 "${SRC}/skills/knowledge-promote/SKILL.md" "${staging}/SKILL.md"
for path in demarkus-knowledge.ts SKILL.md assets/package.json assets/commands assets/context assets/scripts/bootstrap.sh; do
  [[ -s "${staging}/${path}" || -d "${staging}/${path}" ]] \
    || { echo "[demarkus-knowledge] install: staging incomplete (${path})" >&2; exit 1; }
done

# Install assets first and adapter last. A loaded old adapter can consume new
# assets, while a new adapter must never observe an incomplete asset tree.
mkdir -p "${OPENCODE_DIR}/plugins" "${SKILL_DEST}"
previous_assets="${ASSETS_DEST}.prev.$$"
rm -rf "${previous_assets}"
[[ -e "${ASSETS_DEST}" ]] && mv "${ASSETS_DEST}" "${previous_assets}"
if ! mv "${staging}/assets" "${ASSETS_DEST}"; then
  if [[ -e "${previous_assets}" ]] && mv "${previous_assets}" "${ASSETS_DEST}"; then
    echo "[demarkus-knowledge] install: asset swap failed; previous assets restored" >&2
  else
    echo "[demarkus-knowledge] install: asset swap and restore failed; inspect ${previous_assets}" >&2
  fi
  exit 1
fi
install -m 0644 "${staging}/SKILL.md" "${SKILL_DEST}/SKILL.md"
install -m 0644 "${staging}/demarkus-knowledge.ts" "${PLUGIN_DEST}"
rm -rf "${previous_assets}" "${staging}"
previous_assets=""
staging=""

echo "[demarkus-knowledge] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Restart OpenCode, then run /knowledge-join <broker-url>."
