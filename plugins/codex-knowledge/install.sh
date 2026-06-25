#!/usr/bin/env bash
# Install demarkus-knowledge into Codex. Codex has no plugin package manager —
# extensions are config.toml entries (hooks) plus skills — so this wires those up
# idempotently:
#
#   1. bootstrap the shared demarkus-plugin binary (~/.demarkus/bin)
#   2. render config.example.toml (fill @PLUGIN_DIR@) into a marker-delimited
#      MANAGED block in ~/.codex/config.toml (re-runs replace it)
#   3. install the /knowledge* skills into ~/.agents/skills/
#
# Skills (not the deprecated ~/.codex/prompts) are the supported mechanism in
# current Codex — invoke them with `/skills`, a `$skill-name` mention, or let
# Codex pick them implicitly. Knowledge systems themselves are added per-join by
# /knowledge-join, not here. Re-running is safe. To uninstall, delete the block
# between the two demarkus-knowledge markers and the installed skill directories.

set -euo pipefail

PLUGIN_DIR="$(cd "$(dirname "$0")" && pwd)"
CODEX_DIR="${CODEX_HOME:-${HOME}/.codex}"
CONFIG="${CODEX_DIR}/config.toml"
SKILLS_DIR="${HOME}/.agents/skills"
BEGIN="# >>> demarkus-knowledge (managed by install.sh) >>>"
END="# <<< demarkus-knowledge (managed by install.sh) <<<"

echo "[demarkus-knowledge] installing the shared binary…" >&2
bash "${PLUGIN_DIR}/scripts/bootstrap.sh"

chmod +x "${PLUGIN_DIR}"/hooks/*.sh

mkdir -p "${CODEX_DIR}" "${SKILLS_DIR}"
[[ -f "${CONFIG}" ]] || : > "${CONFIG}"

rendered="$(sed -e "s#@PLUGIN_DIR@#${PLUGIN_DIR}#g" "${PLUGIN_DIR}/config.example.toml")"

# Drop any prior managed block, then append the fresh one. awk keeps everything
# outside the markers untouched, so hand-written config is preserved.
tmp="$(mktemp)"
awk -v b="${BEGIN}" -v e="${END}" '
  $0 == b { skip = 1; next }
  $0 == e { skip = 0; next }
  !skip   { print }
' "${CONFIG}" > "${tmp}"

{
  cat "${tmp}"
  printf '\n%s\n%s\n%s\n' "${BEGIN}" "${rendered}" "${END}"
} > "${CONFIG}"
rm -f "${tmp}"

cp -R "${PLUGIN_DIR}"/skills/. "${SKILLS_DIR}/"

echo "[demarkus-knowledge] wired into ${CONFIG} and installed skills in ${SKILLS_DIR}." >&2
echo "[demarkus-knowledge] join a system with the /knowledge-join skill, then restart Codex." >&2
