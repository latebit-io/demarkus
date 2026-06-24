#!/usr/bin/env bash
# bootstrap.sh — the ONE bash script the demarkus plugins retain. It ensures the
# shared demarkus-plugin binary is installed at the pinned version, then exits.
# Everything else (provisioning the server, gates, nudges, guidance, registry)
# lives in that binary. A harness's session-start runs this, then calls
# `demarkus-plugin provision` (memory) / `demarkus-plugin guidance` (knowledge).
#
# Chicken-and-egg: you need the binary to run the binary, so this tiny installer
# is irreducible. It carries only the demarkus-plugin version pin; the binary
# itself pins + downloads the server/mcp/token.

set -euo pipefail

TOOLS_VERSION="0.3.0"   # demarkus-plugin ships in the tools/ release
BIN_DIR="${HOME}/.demarkus/bin"
BIN="${BIN_DIR}/demarkus-plugin"

# Already at the pinned version? Nothing to do.
if [[ -x "${BIN}" ]] && [[ "$("${BIN}" version 2>/dev/null || true)" == "${TOOLS_VERSION}" ]]; then
  exit 0
fi

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux)  os="linux" ;;
  *) echo "[demarkus] bootstrap: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64)        arch="amd64" ;;
  *) echo "[demarkus] bootstrap: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
plat="${os}_${arch}"

# Preflight every tool the install path needs, BEFORE any network I/O, so a host
# missing one fails with an actionable message instead of a bare error mid-way.
command -v curl    >/dev/null 2>&1 || { echo "[demarkus] bootstrap: curl required" >&2; exit 1; }
command -v tar     >/dev/null 2>&1 || { echo "[demarkus] bootstrap: tar required" >&2; exit 1; }
command -v install >/dev/null 2>&1 || { echo "[demarkus] bootstrap: install(1) required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || { echo "[demarkus] bootstrap: sha256sum or shasum required" >&2; exit 1; }

mkdir -p "${BIN_DIR}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

base="https://github.com/latebit-io/demarkus/releases/download/tools%2Fv${TOOLS_VERSION}"
arc="demarkus-plugin_${TOOLS_VERSION}_${plat}.tar.gz"
curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 -o "${tmp}/${arc}"  "${base}/${arc}"                        || { echo "[demarkus] bootstrap: download ${arc} failed" >&2; exit 1; }
curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 -o "${tmp}/sums"     "${base}/demarkus-tools_checksums.txt"  || { echo "[demarkus] bootstrap: download checksums failed" >&2; exit 1; }

expected="$(grep " ${arc}\$" "${tmp}/sums" | awk '{print $1}')"
[[ -n "${expected}" ]] || { echo "[demarkus] bootstrap: no checksum entry for ${arc}" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp}/${arc}" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "${tmp}/${arc}" | awk '{print $1}')"
fi
[[ "${expected}" == "${actual}" ]] || { echo "[demarkus] bootstrap: checksum mismatch for ${arc}" >&2; exit 1; }

tar -xzf "${tmp}/${arc}" -C "${tmp}"
install -m 0755 "${tmp}/demarkus-plugin" "${BIN}"
echo "[demarkus] bootstrap: installed demarkus-plugin v${TOOLS_VERSION}" >&2
