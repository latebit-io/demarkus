#!/usr/bin/env bash
# Install the pinned demarkus-plugin helper. Knowledge uses no local server,
# but gate, guidance, registry, nudge, and update behavior live in this binary.

set -euo pipefail

TOOLS_VERSION="0.29.0"
BIN_DIR="${HOME}/.demarkus/bin"
BIN="${BIN_DIR}/demarkus-plugin"

version_at_least() {
  local have_major have_minor have_patch need_major need_minor need_patch
  IFS=. read -r have_major have_minor have_patch <<<"$1"
  IFS=. read -r need_major need_minor need_patch <<<"$2"
  [[ "${have_major}" =~ ^[0-9]+$ && "${have_minor}" =~ ^[0-9]+$ && "${have_patch}" =~ ^[0-9]+$ ]] || return 1
  ((have_major > need_major)) ||
    ((have_major == need_major && have_minor > need_minor)) ||
    ((have_major == need_major && have_minor == need_minor && have_patch >= need_patch))
}

installed=""
if [[ -x "${BIN}" ]]; then
  if ! installed="$("${BIN}" version 2>&1)"; then
    echo "[demarkus] bootstrap: existing helper version check failed: ${installed}" >&2
    installed=""
  fi
fi
if [[ -n "${installed}" ]] && version_at_least "${installed}" "${TOOLS_VERSION}"; then
  exit 0
fi

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) echo "[demarkus] bootstrap: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch="arm64" ;;
  x86_64) arch="amd64" ;;
  *) echo "[demarkus] bootstrap: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
platform="${os}_${arch}"

command -v curl >/dev/null 2>&1 || { echo "[demarkus] bootstrap: curl required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "[demarkus] bootstrap: tar required" >&2; exit 1; }
command -v install >/dev/null 2>&1 || { echo "[demarkus] bootstrap: install(1) required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || {
  echo "[demarkus] bootstrap: sha256sum or shasum required" >&2
  exit 1
}

mkdir -p "${BIN_DIR}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

base="https://github.com/latebit-io/demarkus/releases/download/tools%2Fv${TOOLS_VERSION}"
archive="demarkus-plugin_${TOOLS_VERSION}_${platform}.tar.gz"
curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 \
  -o "${tmp}/${archive}" "${base}/${archive}" \
  || { echo "[demarkus] bootstrap: download ${archive} failed" >&2; exit 1; }
curl -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2 \
  -o "${tmp}/sums" "${base}/demarkus-tools_checksums.txt" \
  || { echo "[demarkus] bootstrap: download checksums failed" >&2; exit 1; }

expected="$(grep " ${archive}\$" "${tmp}/sums" | awk '{print $1}')"
[[ -n "${expected}" ]] || { echo "[demarkus] bootstrap: no checksum entry for ${archive}" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp}/${archive}" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "${tmp}/${archive}" | awk '{print $1}')"
fi
[[ "${expected}" == "${actual}" ]] || { echo "[demarkus] bootstrap: checksum mismatch for ${archive}" >&2; exit 1; }

tar -xzf "${tmp}/${archive}" -C "${tmp}"
if [[ -d "${BIN}" ]]; then
  echo "[demarkus] bootstrap: ${BIN} is a directory; remove it and retry" >&2
  exit 1
fi
install -m 0755 "${tmp}/demarkus-plugin" "${BIN}.new.$$"
mv -f "${BIN}.new.$$" "${BIN}"
echo "[demarkus] bootstrap: installed demarkus-plugin v${TOOLS_VERSION}" >&2
