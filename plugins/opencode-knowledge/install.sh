#!/usr/bin/env bash
# Install the OpenCode knowledge adapter, native skill, and runtime assets.
# Supports a checkout or curl-piped standalone install.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/opencode"
PLUGIN_DEST="${OPENCODE_DIR}/plugins/demarkus-knowledge.ts"
SKILLS_DIR="${OPENCODE_DIR}/skills"
SKILL_DEST="${SKILLS_DIR}/knowledge-promote"
ASSETS_DEST="${HOME}/.demarkus/opencode-knowledge"
MEMORY_PLUGIN="${OPENCODE_DIR}/plugins/demarkus-memory.ts"
LOCK_DIR="${HOME}/.demarkus/opencode-knowledge.install.lock"
LOCK_TOKEN="owner.$$.$RANDOM"

tmpdir=""
staging=""
previous_assets=""
previous_plugin=""
previous_skill=""
plugin_staged=""
skill_staged=""
deployment_started=false
committed=false
assets_deployed=false
plugin_deployed=false
skill_deployed=false
lock_owned=false

acquire_lock() {
  mkdir -p "${HOME}/.demarkus"
  if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
    echo "[demarkus-knowledge] install: another installer or stale lock exists at ${LOCK_DIR}" >&2
    return 1
  fi
  if ! printf '%s\n' "$$" >"${LOCK_DIR}/${LOCK_TOKEN}"; then
    rmdir "${LOCK_DIR}" \
      || echo "[demarkus-knowledge] install: failed lock cleanup at ${LOCK_DIR}" >&2
    return 1
  fi
  lock_owned=true
}

restore_component() {
  local label="$1" destination="$2" previous="$3" deployed="$4"
  if [[ -e "${previous}" || -L "${previous}" ]]; then
    rm -rf "${destination}" \
      || { echo "[demarkus-knowledge] install: removing failed ${label} deployment failed" >&2; return 1; }
    mv "${previous}" "${destination}" \
      || { echo "[demarkus-knowledge] install: restoring ${label} failed; backup preserved at ${previous}" >&2; return 1; }
  elif [[ "${deployed}" == true ]]; then
    rm -rf "${destination}" \
      || { echo "[demarkus-knowledge] install: removing new ${label} after failure failed" >&2; return 1; }
  fi
}

transactional_uninstall() {
  local suffix="$$.$RANDOM" backup cleanup_failed=false
  previous_plugin="${PLUGIN_DEST}.uninstall.${suffix}"
  previous_skill="${SKILLS_DIR}/.knowledge-promote.uninstall.${suffix}"
  previous_assets="${ASSETS_DEST}.uninstall.${suffix}"
  deployment_started=true

  if [[ -e "${PLUGIN_DEST}" || -L "${PLUGIN_DEST}" ]]; then
    mv "${PLUGIN_DEST}" "${previous_plugin}" \
      || { echo "[demarkus-knowledge] uninstall: moving plugin to backup failed" >&2; return 1; }
  fi
  if [[ -e "${SKILL_DEST}" || -L "${SKILL_DEST}" ]]; then
    mv "${SKILL_DEST}" "${previous_skill}" \
      || { echo "[demarkus-knowledge] uninstall: moving skill to backup failed" >&2; return 1; }
  fi
  if [[ -e "${ASSETS_DEST}" || -L "${ASSETS_DEST}" ]]; then
    mv "${ASSETS_DEST}" "${previous_assets}" \
      || { echo "[demarkus-knowledge] uninstall: moving assets to backup failed" >&2; return 1; }
  fi

  # All active paths are now absent. Backup deletion is cleanup, not rollback:
  # once deletion starts it may partially succeed and cannot be reversed safely.
  committed=true
  for backup in "${previous_plugin}" "${previous_skill}" "${previous_assets}"; do
    if ! rm -rf "${backup}"; then
      echo "[demarkus-knowledge] uninstall: removed active components, but backup cleanup failed at ${backup}" >&2
      cleanup_failed=true
    fi
  done
  if [[ "${cleanup_failed}" == true ]]; then
    return 1
  fi
  previous_plugin=""
  previous_skill=""
  previous_assets=""
}

cleanup() {
  local status=$? rollback_failed=false
  local leftover
  set +e
  if [[ "${deployment_started}" == true && "${committed}" != true ]]; then
    restore_component "plugin" "${PLUGIN_DEST}" "${previous_plugin}" "${plugin_deployed}" || rollback_failed=true
    restore_component "skill" "${SKILL_DEST}" "${previous_skill}" "${skill_deployed}" || rollback_failed=true
    restore_component "assets" "${ASSETS_DEST}" "${previous_assets}" "${assets_deployed}" || rollback_failed=true
  fi
  for leftover in "${tmpdir}" "${staging}" "${plugin_staged}" "${skill_staged}"; do
    [[ -n "${leftover}" ]] || continue
    if ! rm -rf "${leftover}"; then
      echo "[demarkus-knowledge] install: temporary cleanup failed at ${leftover}" >&2
      [[ "${committed}" == true ]] || status=1
    fi
  done
  if [[ "${rollback_failed}" == true ]]; then
    status=1
  fi
  if [[ "${lock_owned}" == true ]]; then
    if [[ ! -f "${LOCK_DIR}/${LOCK_TOKEN}" || -L "${LOCK_DIR}/${LOCK_TOKEN}" ]]; then
      echo "[demarkus-knowledge] install: lock ownership changed; preserving ${LOCK_DIR}" >&2
      status=1
    else
      if ! rm -f "${LOCK_DIR}/${LOCK_TOKEN}" || ! rmdir "${LOCK_DIR}"; then
        echo "[demarkus-knowledge] install: releasing lock ${LOCK_DIR} failed" >&2
        status=1
      fi
    fi
  fi
  trap - EXIT
  exit "${status}"
}
trap cleanup EXIT

acquire_lock

if [[ "${1:-}" == "--uninstall" ]]; then
  transactional_uninstall
  echo "[demarkus-knowledge] uninstalled plugin, skill, and assets. Shared registry and OAuth state remain."
  exit 0
fi

if [[ -e "${MEMORY_PLUGIN}" ]] && ! grep -q 'io.demarkus.opencode.gate-adapters' "${MEMORY_PLUGIN}"; then
  echo "[demarkus-knowledge] install: update demarkus-opencode-memory first; the installed adapter cannot coordinate publish gates" >&2
  echo "  curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash" >&2
  exit 1
fi

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

# Stage each cross-directory component beside its destination so final moves
# are atomic even when XDG_CONFIG_HOME and HOME use different filesystems.
mkdir -p "${OPENCODE_DIR}/plugins" "${SKILLS_DIR}"
plugin_staged="${PLUGIN_DEST}.new.$$"
skill_staged="${SKILL_DEST}.new.$$"
rm -rf "${plugin_staged}" "${skill_staged}"
install -m 0644 "${staging}/demarkus-knowledge.ts" "${plugin_staged}"
mkdir "${skill_staged}"
install -m 0644 "${staging}/SKILL.md" "${skill_staged}/SKILL.md"

previous_assets="${ASSETS_DEST}.prev.$$"
previous_plugin="${PLUGIN_DEST}.prev.$$"
previous_skill="${SKILL_DEST}.prev.$$"
rm -rf "${previous_assets}" "${previous_plugin}" "${previous_skill}"

deployment_started=true
[[ -e "${ASSETS_DEST}" || -L "${ASSETS_DEST}" ]] && mv "${ASSETS_DEST}" "${previous_assets}"
[[ -e "${SKILL_DEST}" || -L "${SKILL_DEST}" ]] && mv "${SKILL_DEST}" "${previous_skill}"
[[ -e "${PLUGIN_DEST}" || -L "${PLUGIN_DEST}" ]] && mv "${PLUGIN_DEST}" "${previous_plugin}"

mv "${staging}/assets" "${ASSETS_DEST}" \
  || { echo "[demarkus-knowledge] install: deploying assets failed" >&2; exit 1; }
assets_deployed=true
mv "${skill_staged}" "${SKILL_DEST}" \
  || { echo "[demarkus-knowledge] install: deploying skill failed" >&2; exit 1; }
skill_deployed=true
mv "${plugin_staged}" "${PLUGIN_DEST}" \
  || { echo "[demarkus-knowledge] install: deploying plugin failed" >&2; exit 1; }
plugin_deployed=true

committed=true
for backup in "${previous_assets}" "${previous_plugin}" "${previous_skill}"; do
  if ! rm -rf "${backup}"; then
    echo "[demarkus-knowledge] install: installed successfully, but old backup cleanup failed at ${backup}" >&2
  fi
done
previous_assets=""
previous_plugin=""
previous_skill=""
plugin_staged=""
skill_staged=""

echo "[demarkus-knowledge] installed:"
echo "  plugin  ${PLUGIN_DEST}"
echo "  skill   ${SKILL_DEST}/SKILL.md"
echo "  assets  ${ASSETS_DEST}"
echo "Restart OpenCode, then run /knowledge-join <broker-url>."
