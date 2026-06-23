#!/usr/bin/env bash
# mirror-policy.sh <slug> — read a knowledge system's policy.md BODY on stdin and
# deterministically mirror its enforced core (strictness / require_tags /
# require_fields) to the per-slug local files the publish gate reads.
#
# Replaces the agent-prose mirroring that /knowledge-join used to do by hand: the
# agent fetches policy.md over MCP (a script can't reach the broker) and pipes the
# body here; the parse + file paths are no longer agent judgment, so a join always
# mirrors the same way. A knob absent from the policy clears its mirror file, so
# relaxing the policy de-enforces. Idempotent.
#
# Usage:  <policy.md body on stdin> | mirror-policy.sh <slug>

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
# shellcheck source=lib.sh
. "${SCRIPT_DIR}/lib.sh"

[[ $# -eq 1 && -n "${1:-}" ]] || die "usage: mirror-policy.sh <slug> (policy.md body on stdin)"
# The slug is interpolated into the mirror file paths, so constrain it to a
# path-safe alphabet (no separators, whitespace, or control chars).
case "$1" in
  *[!A-Za-z0-9._-]*) die "invalid slug '$1': only [A-Za-z0-9._-] allowed (it is embedded in mirror file paths)" ;;
esac

mirror_policy "$1"
log "mirrored policy enforced core for '$1' (strictness/require_tags/require_fields)"
