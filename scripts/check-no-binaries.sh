#!/bin/bash
# No compiled binary is ever committed: builds go to <module>/bin/ or dist/, which
# are ignored. Reads the index, so a binary staged and then deleted on disk still fails.
set -euo pipefail

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Index against the empty tree; numstat marks binary blobs "-", and only
# those need their header read.
git diff --cached --numstat -z "$(git hash-object -t tree /dev/null)" >"$work/numstat"

found=()
while IFS=$'\t' read -r -d '' added _ path; do
  [ "$added" = "-" ] || continue
  git cat-file blob ":$path" >"$work/blob"
  magic=$(od -An -tx1 -N4 "$work/blob" | tr -d ' \n')
  case "$magic" in
  # ELF; Mach-O 32/64 both byte orders, fat; PE/DOS "MZ"
  7f454c46 | cffaedfe | cefaedfe | feedfacf | feedface | cafebabe | 4d5a*)
    found+=("$path")
    ;;
  esac
done <"$work/numstat"

if [ "${#found[@]}" -gt 0 ]; then
  echo "Compiled binaries are staged or tracked; build with -o bin/<name> and git rm --cached them:" >&2
  printf '  %s\n' "${found[@]}" >&2
  exit 1
fi
echo "✓ No tracked binaries"
