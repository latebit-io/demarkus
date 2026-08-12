#!/bin/bash
# Enforces the 1-3 line comment rule (CLAUDE.md) on comment blocks touched by
# the current change: committed on this branch since main, or uncommitted.
# Pre-existing long blocks elsewhere are left alone; tighten on touch.
set -euo pipefail

# Fail closed: an unresolvable base would silently skip committed changes.
if ! base=$(git merge-base main HEAD 2>/dev/null); then
  echo "check-comment-length: cannot resolve merge-base with main" >&2
  exit 1
fi

files=$(git diff --name-only "$base" -- '*.go' | sort -u)
status=0

for file in $files; do
  [ -f "$file" ] || continue
  # Touched lines in the new file: added lines, plus the line at (and after)
  # each deletion-only hunk so shrunk-but-still-long blocks are checked too.
  touched=$(git diff -U0 "$base" -- "$file" | awk '
    /^@@/ {
      if (match($0, /\+[0-9]+(,[0-9]+)?/)) {
        split(substr($0, RSTART + 1, RLENGTH - 1), a, ",")
        len = (a[2] == "" ? 1 : a[2])
        if (len == 0) { printf "%d %d ", (a[1] < 1 ? 1 : a[1]), a[1] + 1 }
        for (i = 0; i < len; i++) printf "%d ", a[1] + i
      }
    }')
  [ -n "${touched// /}" ] || continue

  out=$(awk -v touched="$touched" '
    BEGIN { n = split(touched, arr, " "); for (i = 1; i <= n; i++) hit[arr[i]] = 1 }
    # A block directly above a package clause is the package doc comment;
    # godoc convention allows length there.
    function flush(cur) {
      if (run > 3 && marked && cur !~ /^package /)
        print FILENAME ":" startln ": comment block of " run " lines (max 3; move deep rationale to a doc)"
      run = 0; marked = 0
    }
    {
      t = $0; sub(/^[ \t]+/, "", t)
      if (t ~ /^\/\// && t !~ /^\/\/go:/ && t !~ /^\/\/nolint/) {
        if (run == 0) startln = NR
        run++
        if (hit[NR]) marked = 1
        next
      }
      flush(t)
    }
    END { flush("") }
  ' "$file")

  if [ -n "$out" ]; then
    echo "$out"
    status=1
  fi
done

exit $status
