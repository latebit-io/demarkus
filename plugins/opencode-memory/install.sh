#!/usr/bin/env bash
# Install the demarkus-memory OpenCode plugin: plugin file into OpenCode's
# auto-loaded plugins dir, assets into ~/.demarkus/opencode-memory/. Works from
# a checkout or standalone via curl | bash (downloads the repo tarball).
#
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
#
# Idempotent; re-run to update; --uninstall removes. DEMARKUS_REF pins the
# standalone install to a branch, tag, or full commit SHA (default main;
# pass a SHA or tag for a reproducible install).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/opencode"
PLUGIN_DEST="${OPENCODE_DIR}/plugins/demarkus-memory.ts"
SKILL_DEST="${OPENCODE_DIR}/skills/memory"
ASSETS_DEST="${HOME}/.demarkus/opencode-memory"

if [[ "${1:-}" == "--uninstall" ]]; then
  rm -f "${PLUGIN_DEST}"
  rm -rf "${SKILL_DEST}" "${ASSETS_DEST}"
  echo "[demarkus-memory] uninstalled (plugin, skill, assets). ~/.demarkus state and binaries untouched."
  exit 0
fi

tmpdir=""
staging=""
prev=""
# On any exit: restore the backed-up asset tree if the swap never completed,
# then drop temp dirs.
cleanup() {
  if [[ -n "${prev}" && -e "${prev}" && ! -e "${ASSETS_DEST}" ]]; then
    mv "${prev}" "${ASSETS_DEST}" \
      || echo "[demarkus-memory] install: restoring previous assets failed; backup preserved at ${prev}" >&2
  fi
  rm -rf "${tmpdir}" "${staging}"
}
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
  # codeload accepts a branch, tag, or commit SHA as the trailing ref.
  curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 \
    "https://codeload.github.com/latebit-io/demarkus/tar.gz/${REF}" \
    | tar -xz -C "${tmpdir}" "${topdir}/plugins/opencode-memory" \
    || { echo "[demarkus-memory] install: download/extract failed for ref ${REF}" >&2; exit 1; }
  SRC="${tmpdir}/${topdir}/plugins/opencode-memory"
fi

for d in src/demarkus-memory.ts package.json commands context scripts skills/memory/SKILL.md; do
  [[ -e "${SRC}/${d}" ]] || { echo "[demarkus-memory] install: missing ${d} in ${SRC}" >&2; exit 1; }
done

# Stage EVERYTHING first, then commit: a failure mid-install must leave the
# previous installation intact and never a mix of old and new revisions.
staging="${ASSETS_DEST}.staging.$$"
rm -rf "${staging}"
mkdir -p "${staging}/assets"
for d in commands context scripts; do
  cp -R "${SRC}/${d}" "${staging}/assets/${d}"
done
chmod 0755 "${staging}/assets/scripts/"*.sh
# The manifest travels with the assets: the update check reads its version, and
# OpenCode's flat plugin file has nowhere else to carry one.
install -m 0644 "${SRC}/package.json" "${staging}/assets/package.json"
install -m 0644 "${SRC}/src/demarkus-memory.ts" "${staging}/demarkus-memory.ts"
install -m 0644 "${SRC}/skills/memory/SKILL.md" "${staging}/SKILL.md"
for f in demarkus-memory.ts SKILL.md assets/package.json assets/commands assets/context assets/scripts/bootstrap.sh; do
  [[ -s "${staging}/${f}" || -d "${staging}/${f}" ]] || { echo "[demarkus-memory] install: staging incomplete (${f})" >&2; exit 1; }
done

# Commit: per-file renames are atomic; the asset tree swaps via a backup that
# is restored if the new tree cannot be moved into place.
mkdir -p "${OPENCODE_DIR}/plugins" "${SKILL_DEST}"
mv -f "${staging}/demarkus-memory.ts" "${PLUGIN_DEST}"
mv -f "${staging}/SKILL.md" "${SKILL_DEST}/SKILL.md"
prev="${ASSETS_DEST}.prev.$$"
rm -rf "${prev}"
[[ -e "${ASSETS_DEST}" ]] && mv "${ASSETS_DEST}" "${prev}"
if mv "${staging}/assets" "${ASSETS_DEST}"; then
  rm -rf "${prev}" "${staging}"
  staging=""
else
  if [[ ! -e "${prev}" ]]; then
    echo "[demarkus-memory] install: asset swap failed (fresh install, nothing to restore)" >&2
  elif mv "${prev}" "${ASSETS_DEST}"; then
    echo "[demarkus-memory] install: asset swap failed; previous assets restored" >&2
  else
    echo "[demarkus-memory] install: asset swap failed AND restore failed; backup preserved at ${prev}" >&2
  fi
  exit 1
fi

echo "[demarkus-memory] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Start (or restart) opencode; the first session provisions the memory. Diagnose with /memory-status."
