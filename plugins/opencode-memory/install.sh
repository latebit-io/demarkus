#!/usr/bin/env bash
# Install the demarkus-memory OpenCode plugin: plugin file into OpenCode's
# auto-loaded plugins dir, assets into ~/.demarkus/opencode-memory/. Works from
# a checkout or standalone via curl | bash (downloads the repo tarball).
#
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
#
# Idempotent; re-run to update; --uninstall removes. DEMARKUS_REF pins the
# git ref for standalone installs (default main).

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

tmpdir=""
staging=""
cleanup() { rm -rf "${tmpdir}" "${staging}"; }
trap cleanup EXIT

# Source resolution: a checkout when run next to the plugin files, otherwise
# (curl | bash) download the repo tarball and install from it.
SRC="${HERE}"
if [[ ! -e "${SRC}/src/demarkus-memory.ts" ]]; then
  REF="${DEMARKUS_REF:-main}"
  command -v curl >/dev/null 2>&1 || { echo "[demarkus-memory] install: curl required" >&2; exit 1; }
  command -v tar  >/dev/null 2>&1 || { echo "[demarkus-memory] install: tar required" >&2; exit 1; }
  tmpdir="$(mktemp -d)"
  topdir="demarkus-${REF//\//-}"
  echo "[demarkus-memory] downloading plugin (ref ${REF})..." >&2
  curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 \
    "https://codeload.github.com/latebit-io/demarkus/tar.gz/refs/heads/${REF}" \
    | tar -xz -C "${tmpdir}" "${topdir}/plugins/opencode-memory" \
    || { echo "[demarkus-memory] install: download/extract failed for ref ${REF}" >&2; exit 1; }
  SRC="${tmpdir}/${topdir}/plugins/opencode-memory"
fi

for d in src/demarkus-memory.ts commands context scripts skills/soul-memory/SKILL.md; do
  [[ -e "${SRC}/${d}" ]] || { echo "[demarkus-memory] install: missing ${d} in ${SRC}" >&2; exit 1; }
done

mkdir -p "${OPENCODE_DIR}/plugins" "${SKILL_DEST}"
install -m 0644 "${SRC}/src/demarkus-memory.ts" "${PLUGIN_DEST}"
install -m 0644 "${SRC}/skills/soul-memory/SKILL.md" "${SKILL_DEST}/SKILL.md"

# Stage the full asset tree, then swap it in: an interrupted copy must not
# leave the active install with missing commands/context/scripts.
staging="${ASSETS_DEST}.staging.$$"
rm -rf "${staging}"
mkdir -p "${staging}"
for d in commands context scripts; do
  cp -R "${SRC}/${d}" "${staging}/${d}"
done
chmod 0755 "${staging}/scripts/"*.sh
rm -rf "${ASSETS_DEST}"
mv "${staging}" "${ASSETS_DEST}"
staging=""

echo "[demarkus-memory] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Start (or restart) opencode; the first session provisions the soul. Diagnose with /soul-status."
