#!/usr/bin/env bash
# Install demarkus-memory into Codex. Codex has no package manager for plugins —
# extensions are config.toml entries (mcp_servers + hooks) plus skills — so this
# script wires those up idempotently:
#
#   1. bootstrap the shared demarkus-plugin binary (~/.demarkus/bin)
#   2. render config.example.toml (fill @BIN@ / @PLUGIN_DIR@) into a
#      marker-delimited MANAGED block in ~/.codex/config.toml (re-runs replace it)
#   3. install the /soul-* and /promote* skills into ~/.agents/skills/
#
# Skills (not the deprecated ~/.codex/prompts) are the supported mechanism in
# current Codex — invoke them with `/skills`, a `$skill-name` mention, or let
# Codex pick them implicitly by description. Re-running is safe: the managed block
# is replaced and skills are overwritten. To uninstall, delete the block between
# the two demarkus-memory markers and the installed skill directories.

set -euo pipefail

PLUGIN_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${HOME}/.demarkus/bin/demarkus-plugin"
CODEX_DIR="${CODEX_HOME:-${HOME}/.codex}"
CONFIG="${CODEX_DIR}/config.toml"
SKILLS_DIR="${HOME}/.agents/skills"
BEGIN="# >>> demarkus-memory (managed by install.sh) >>>"
END="# <<< demarkus-memory (managed by install.sh) <<<"

echo "[demarkus-memory] installing the shared binary…" >&2
bash "${PLUGIN_DIR}/scripts/bootstrap.sh"

chmod +x "${PLUGIN_DIR}"/hooks/*.sh

mkdir -p "${CODEX_DIR}" "${SKILLS_DIR}"
[[ -f "${CONFIG}" ]] || : > "${CONFIG}"

# Render the wiring with absolute paths.
rendered="$(sed -e "s#@BIN@#${BIN}#g" -e "s#@PLUGIN_DIR@#${PLUGIN_DIR}#g" "${PLUGIN_DIR}/config.example.toml")"

# Drop any prior managed block, then append the fresh one. awk keeps everything
# outside the markers untouched, so hand-written config is preserved.
tmp="$(mktemp)"
awk -v b="${BEGIN}" -v e="${END}" '
  $0 == b { skip = 1; next }
  $0 == e { skip = 0; next }
  !skip   { print }
' "${CONFIG}" > "${tmp}"

# Trim a trailing blank run, then append the block with one separating blank line.
{
  cat "${tmp}"
  printf '\n%s\n%s\n%s\n' "${BEGIN}" "${rendered}" "${END}"
} > "${CONFIG}"
rm -f "${tmp}"

# Install the skills (each is a <name>/SKILL.md dir; Codex reads ~/.agents/skills).
cp -R "${PLUGIN_DIR}"/skills/. "${SKILLS_DIR}/"

echo "[demarkus-memory] wired into ${CONFIG} and installed skills in ${SKILLS_DIR}." >&2
echo "[demarkus-memory] restart Codex (or start a new session) to load the MCP server, hooks, and skills." >&2
