#!/bin/bash
# Enforces the 1-3 line comment rule (CLAUDE.md) on comment blocks touched by
# the current change: committed on this branch since main, or uncommitted.
# Pre-existing long blocks elsewhere are left alone; tighten on touch.
set -euo pipefail

base=$(git merge-base main HEAD 2>/dev/null || git rev-parse HEAD)
status=0

while IFS= read -r file; do
  [ -f "$file" ] || continue
  added=$(git diff -U0 "$base" -- "$file" | awk '
    /^@@/ {
      if (match($0, /\+[0-9]+(,[0-9]+)?/)) {
        split(substr($0, RSTART + 1, RLENGTH - 1), a, ",")
        len = (a[2] == "" ? 1 : a[2])
        for (i = 0; i < len; i++) printf "%d ", a[1] + i
      }
    }')
  [ -n "${added// /}" ] || continue

  out=$(awk -v added="$added" '
    BEGIN { n = split(added, arr, " "); for (i = 1; i <= n; i++) isadd[arr[i]] = 1 }
    # A block directly above a package clause is the package doc comment;
    # godoc convention allows length there.
    function flush(cur) {
      if (run > 3 && touched && cur !~ /^package /)
        print FILENAME ":" startln ": comment block of " run " lines (max 3; move deep rationale to a doc)"
      run = 0; touched = 0
    }
    {
      t = $0; sub(/^[ \t]+/, "", t)
      if (t ~ /^\/\// && t !~ /^\/\/go:/ && t !~ /^\/\/nolint/) {
        if (run == 0) startln = NR
        run++
        if (isadd[NR]) touched = 1
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
done < <(git diff --name-only "$base" -- '*.go' | sort -u)

exit $status
