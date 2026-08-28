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

# One tree-wide diff so rename detection pairs moved files; a pure
# rename then has no hunks and stays untouched, per tighten-on-touch.
files_and_lines=$(git diff -U0 -M "$base" -- '*.go' | awk '
  /^\+\+\+ \/dev\/null/ { file = ""; next }
  /^\+\+\+ b\// { file = substr($0, 7); next }
  /^@@/ {
    if (file != "" && match($0, /\+[0-9]+(,[0-9]+)?/)) {
      split(substr($0, RSTART + 1, RLENGTH - 1), a, ",")
      len = (a[2] == "" ? 1 : a[2])
      # Deletion-only hunk: mark the line at and after it so
      # shrunk-but-still-long blocks are checked too.
      if (len == 0) { printf "%s %d\n%s %d\n", file, (a[1] < 1 ? 1 : a[1]), file, a[1] + 1 }
      for (i = 0; i < len; i++) printf "%s %d\n", file, a[1] + i
    }
  }')
[ -n "$files_and_lines" ] || exit 0

files=()
while IFS= read -r f; do
  [ -f "$f" ] && files+=("$f")
done < <(printf '%s\n' "$files_and_lines" | cut -d' ' -f1 | sort -u)
[ "${#files[@]}" -gt 0 ] || exit 0

out=$(printf '%s\n' "$files_and_lines" | awk '
  NR == FNR { hit[$1, $2] = 1; next }
  # A block directly above a package clause is the package doc comment;
  # godoc convention allows length there.
  function flush(cur) {
    if (run > 3 && marked && cur !~ /^package /)
      print fname ":" startln ": comment block of " run " lines (max 3; move deep rationale to a doc)"
    run = 0; marked = 0
  }
  FNR == 1 { flush(""); fname = FILENAME }
  {
    t = $0; sub(/^[ \t]+/, "", t)
    if (t ~ /^\/\// && t !~ /^\/\/go:/ && t !~ /^\/\/nolint/) {
      if (run == 0) startln = FNR
      run++
      if (hit[FILENAME, FNR]) marked = 1
      next
    }
    flush(t)
  }
  END { flush("") }
' - "${files[@]}")

if [ -n "$out" ]; then
  echo "$out"
  exit 1
fi
