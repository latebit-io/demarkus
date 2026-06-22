#!/usr/bin/env bash
# detect-manual-souls.sh — report hand-wired remote demarkus souls so /soul-join
# can offer to adopt them into managed config.
#
# A "manual soul" is an MCP server that invokes demarkus-mcp DIRECTLY against a
# remote mark:// host — i.e. a soul wired by editing .mcp.json by hand, with its
# token sitting inline in the config (and in `claude mcp list` output). Entries
# that go through our launch wrappers (soul-remote-wrapper.sh / mcp-wrapper.sh)
# are already managed and are NOT reported.
#
# Output:
#   "NONE"                     — nothing to adopt (or `claude` unavailable)
#   "MANUAL" then one row each: "<name>\t<host>\t<insecure 0|1>\t<has-token 0|1>"
#
# The token VALUE is deliberately never emitted — only its presence — so it does
# not leak into the agent transcript. Adoption re-joins with the user supplying
# the token (they have it in their existing entry).
#
# Detection reads `claude mcp list` (the live, scope-merged registry) rather than
# parsing .mcp.json files: it is authoritative and avoids a hand-rolled JSON
# parser. The list format is a human one, so this is best-effort by nature.

set -uo pipefail

if ! command -v claude >/dev/null 2>&1; then
  echo "NONE"
  exit 0
fi

# `claude mcp list` runs a health check and prints a header + one line per
# server: "<name>: <command...> - <status>". We only care about lines whose
# command runs demarkus-mcp directly against a mark:// host.
LIST="$(claude mcp list 2>/dev/null || true)"

ROWS="$(printf '%s\n' "${LIST}" | awk '
  # Skip our managed wrappers and the local-soul wrapper outright.
  /soul-remote-wrapper\.sh/ { next }
  /mcp-wrapper\.sh/         { next }
  # Must invoke demarkus-mcp directly against a remote mark:// host.
  /demarkus-mcp/ && /mark:\/\// {
    line = $0
    # name = text before the first ": "
    ci = index(line, ": ")
    if (ci == 0) next
    name = substr(line, 1, ci - 1)
    cmd  = substr(line, ci + 2)

    # host = the token following "-host "
    host = ""
    if (match(cmd, /-host[ =]+mark:\/\/[^ ]+/)) {
      h = substr(cmd, RSTART, RLENGTH)
      sub(/^-host[ =]+/, "", h)
      host = h
    }
    if (host == "") next

    insecure = (cmd ~ /(^| )-insecure( |$)/) ? "1" : "0"
    hastoken = (cmd ~ /(^| )-token([ =]|$)/) ? "1" : "0"

    printf "%s\t%s\t%s\t%s\n", name, host, insecure, hastoken
  }
')"

if [[ -z "${ROWS}" ]]; then
  echo "NONE"
  exit 0
fi

echo "MANUAL"
printf '%s\n' "${ROWS}"
