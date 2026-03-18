#!/bin/bash
# Publish all .md files from a source directory into the demarkus content store.
#
# Usage:
#   ./publish-docs.sh /tmp/demarkus-docs
#   ./publish-docs.sh /tmp/demarkus-docs /srv/demarkus/content
set -euo pipefail

SRC="${1:?Usage: $0 <source-dir> [content-root]}"
ROOT="${2:-/srv/demarkus/content}"

if [ ! -d "$SRC" ]; then
  echo "error: source directory not found: $SRC" >&2
  exit 1
fi

count=0
for f in $(find "$SRC" -name '*.md' | sort); do
  path="${f#"$SRC"}"
  demarkus-publish -root "$ROOT" -path "$path" < "$f"
  count=$((count + 1))
done

echo ""
echo "published $count documents to $ROOT"
