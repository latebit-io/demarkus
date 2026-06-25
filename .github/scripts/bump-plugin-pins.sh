#!/usr/bin/env bash
#
# Resolve the latest released server/client/tools versions and update the demarkus
# plugins' binary pins to match. Since the Approach-C refactor the pins live in
# TWO places that must stay in lockstep:
#
#   1. tools/demarkus-plugin/internal/provision/provision.go — the Go consts
#      serverVersion / clientVersion / toolsVersion (source of truth; what the
#      binary provisions and self-updates to).
#   2. plugins/*/scripts/bootstrap.sh — TOOLS_VERSION (the chicken-and-egg pin
#      the tiny installer needs to fetch the demarkus-plugin binary itself).
#
# toolsVersion and every bootstrap TOOLS_VERSION MUST match, or provision and
# bootstrap fight over which demarkus-plugin to install.
#
# It also patch-bumps the affected plugins' versions, in two lineages:
#   - memory    (changes when server|client|tools change): claude-code +
#     pi-memory + the claude-code marketplace entry.
#   - knowledge (changes when tools changes, via its bootstrap): claude-code-
#     knowledge + pi-knowledge + the claude-code-knowledge marketplace entry.
# (The Codex plugins carry no version file — only a bootstrap pin.)
#
# Idempotent. Writes "changed=true" plus the resolved versions to $GITHUB_OUTPUT
# (and stdout) when it edited files, "changed=false" otherwise. Requires gh + jq.
#
# Env: GH_REPO (default latebit-io/demarkus), GH_TOKEN for gh.
set -euo pipefail

repo="${GH_REPO:-latebit-io/demarkus}"
provision="tools/demarkus-plugin/internal/provision/provision.go"
marketplace=".claude-plugin/marketplace.json"

# latest <module> -> "X.Y.Z" (highest semver among that module's release tags).
latest() {
  gh release list --repo "$repo" --limit 200 \
    | grep -oE "$1/v[0-9]+\.[0-9]+\.[0-9]+" \
    | sed "s#$1/v##" | sort -V | tail -1
}

# goconst <NAME> -> current value of `NAME = "..."` in provision.go. Exits non-
# zero if the const is missing or not a semver, so a parse failure can't silently
# leave the canonical pin stale while the rest of the bump proceeds.
goconst() {
  local value
  value=$(sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"([^\"]+)\".*/\1/p" "$provision")
  [[ "$value" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "error: missing or invalid $1 in $provision" >&2
    exit 1
  }
  printf '%s\n' "$value"
}

emit() {
  echo "$1=$2"
  [[ -n "${GITHUB_OUTPUT:-}" ]] && echo "$1=$2" >>"$GITHUB_OUTPUT"
  return 0
}

# set_goconst <NAME> <value> — portable in-place edit (temp file; no sed -i).
# Verifies the const existed before and reads back as <value> after, so a failed
# replacement (typo'd name, reformatted file) aborts instead of shipping a stale pin.
set_goconst() {
  local tmp
  goconst "$1" >/dev/null # aborts if the const is absent/malformed
  tmp=$(mktemp)
  sed -E "s/^([[:space:]]*$1[[:space:]]*=[[:space:]]*)\"[^\"]+\"/\1\"$2\"/" "$provision" >"$tmp" && mv "$tmp" "$provision"
  [[ "$(goconst "$1")" == "$2" ]] || {
    echo "error: failed to update $1 to $2 in $provision" >&2
    exit 1
  }
}

# set_bootstrap_tools <value> — update TOOLS_VERSION in every plugin bootstrap.
set_bootstrap_tools() {
  local f tmp
  for f in plugins/*/scripts/bootstrap.sh; do
    [[ -f "$f" ]] || continue
    tmp=$(mktemp)
    cp -p "$f" "$tmp" # preserve the executable bit (bootstrap.sh is 0755)
    sed -E "s/^(TOOLS_VERSION=)\"[^\"]+\"/\1\"$1\"/" "$f" >"$tmp" && mv "$tmp" "$f"
  done
}

# bump_json_version <file> — patch-bump the .version field in a JSON file. Echoes
# the new version. Uses jq (these files round-trip cleanly through it).
bump_json_version() {
  local f cur new tmp; f="$1"
  cur=$(jq -r '.version' "$f")
  new=$(awk -F. '{printf "%d.%d.%d", $1, $2, $3 + 1}' <<<"$cur")
  tmp=$(mktemp)
  jq --arg v "$new" '.version = $v' "$f" >"$tmp" && mv "$tmp" "$f"
  echo "$new"
}

# set_marketplace_version <source-path> <value> — set the version of the
# marketplace entry whose "source" is <source-path>, preserving hand formatting.
set_marketplace_version() {
  local tmp; tmp=$(mktemp)
  # awk exits 42 unless EXACTLY one entry matched the source and got its version
  # updated, so a source typo or reordered field fails loudly instead of leaving
  # the marketplace stale (or editing the wrong entry) while we emit a bumped version.
  awk -v v="$2" -v src="$1" '
    index($0, "\"source\": \"" src "\"") { inblock = 1; seen++ }
    inblock && /"version":/ { sub(/"version": "[^"]*"/, "\"version\": \"" v "\""); inblock = 0; updated++ }
    inblock && /^[[:space:]]*}/ { inblock = 0 }
    { print }
    END { if (seen != 1 || updated != 1) exit 42 }
  ' "$marketplace" >"$tmp" && mv "$tmp" "$marketplace" || {
    rm -f "$tmp"
    echo "error: expected exactly one marketplace entry for source '$1' (matched=${seen:-?})" >&2
    exit 1
  }
}

new_server=$(latest server)
new_client=$(latest client)
new_tools=$(latest tools)
if [[ -z "$new_server" || -z "$new_client" || -z "$new_tools" ]]; then
  echo "error: could not resolve latest server/client/tools versions" >&2
  exit 1
fi

cur_server=$(goconst serverVersion)
cur_client=$(goconst clientVersion)
cur_tools=$(goconst toolsVersion)

emit server "$new_server"
emit client "$new_client"
emit tools "$new_tools"

if [[ "$new_server" == "$cur_server" && "$new_client" == "$cur_client" && "$new_tools" == "$cur_tools" ]]; then
  emit changed false
  echo "pins already current (server=$cur_server client=$cur_client tools=$cur_tools)"
  exit 0
fi

# Update the Go consts (source of truth) and every bootstrap's TOOLS_VERSION.
set_goconst serverVersion "$new_server"
set_goconst clientVersion "$new_client"
set_goconst toolsVersion "$new_tools"
[[ "$new_tools" != "$cur_tools" ]] && set_bootstrap_tools "$new_tools"

# Patch-bump the affected lineages.
memory_changed=false
knowledge_changed=false
[[ "$new_server" != "$cur_server" || "$new_client" != "$cur_client" || "$new_tools" != "$cur_tools" ]] && memory_changed=true
[[ "$new_tools" != "$cur_tools" ]] && knowledge_changed=true

if [[ "$memory_changed" == true ]]; then
  mv_ver="$(bump_json_version plugins/claude-code/.claude-plugin/plugin.json)"
  set_marketplace_version "./plugins/claude-code" "$mv_ver"
  _=$(bump_json_version plugins/pi-memory/package.json)
  emit memory_version "$mv_ver"
fi
if [[ "$knowledge_changed" == true ]]; then
  kv_ver="$(bump_json_version plugins/claude-code-knowledge/.claude-plugin/plugin.json)"
  set_marketplace_version "./plugins/claude-code-knowledge" "$kv_ver"
  _=$(bump_json_version plugins/pi-knowledge/package.json)
  emit knowledge_version "$kv_ver"
fi

emit changed true
echo "bumped: server $cur_server->$new_server, client $cur_client->$new_client, tools $cur_tools->$new_tools"
