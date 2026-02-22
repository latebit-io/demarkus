#!/usr/bin/env bash
#
# Publishes the local docs site (docs/site) to a running Demarkus server.
#
# Usage:
#   ./scripts/seed-docs.sh [host:port] [auth-token] [base-path]
#
# Defaults:
#   host:port  = localhost:6309
#   auth-token = $DEMARKUS_AUTH (or empty)
#   base-path  = /docs
#
# Notes:
# - This script mirrors docs/site/**/*.md to the server under base-path.
# - Each directory's index.md is preserved (e.g. docs/site/install/index.md -> /docs/install/index.md).

set -euo pipefail

HOST="${1:-localhost:6309}"
TOKEN="${2:-${DEMARKUS_AUTH:-}}"
BASE_PATH="${3:-/docs}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
DOCS_DIR="${ROOT_DIR}/docs/site"
DEMARKUS="${ROOT_DIR}/client/bin/demarkus"

if [[ ! -d "${DOCS_DIR}" ]]; then
  echo "Docs directory not found: ${DOCS_DIR}"
  exit 1
fi

if [[ ! -x "${DEMARKUS}" ]]; then
  echo "Building client..."
  make -C "${ROOT_DIR}" client
fi

publish() {
  local path="$1"
  local file="$2"
  local args=(-X PUBLISH -insecure -body "$(cat "$file")")
  if [[ -n "${TOKEN}" ]]; then
    args+=(-auth "${TOKEN}")
  fi
  echo "  PUBLISH ${path}"
  local output
  output=$("${DEMARKUS}" "${args[@]}" "mark://${HOST}${path}" 2>&1)
  echo "${output%%$'\n'*}"
}

echo "Seeding docs site to ${HOST}${BASE_PATH}..."
echo

# Find and publish all markdown files under docs/site.
# Use a stable order for deterministic output.
while IFS= read -r file; do
  rel="${file#${DOCS_DIR}/}"
  server_path="${BASE_PATH}/${rel}"
  # Ensure no accidental double slashes.
  server_path="${server_path//\/\//\/}"
  publish "${server_path}" "${file}"
done < <(find "${DOCS_DIR}" -type f -name "*.md" | sort)

echo
echo "Done! Seeded docs on ${HOST}${BASE_PATH}."
echo
echo "Try fetching:"
echo "  ${DEMARKUS} --insecure mark://${HOST}${BASE_PATH}/index.md"
echo
echo "Crawl the graph:"
echo "  ${DEMARKUS} graph -insecure -depth 2 mark://${HOST}${BASE_PATH}/index.md"
