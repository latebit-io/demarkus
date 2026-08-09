#!/usr/bin/env bash
#
# Resolve the latest released server/client/tools versions and update the demarkus
# plugins' binary pins to match. The pins live in:
#
#   1. tools/demarkus-plugin/internal/provision/provision.go — serverVersion /
#      clientVersion (the server & mcp binaries provision downloads) and
#      fallbackToolsVersion (the tools release a DEV build pulls demarkus-token
#      from; a real build uses its own ldflags version).
#   2. plugins/*/scripts/bootstrap.sh — TOOLS_VERSION (the chicken-and-egg pin
#      the tiny installer uses to fetch the demarkus-plugin binary itself).
#
# provision no longer re-downloads demarkus-plugin (bootstrap owns it) and derives
# the token release from the running binary's own version, so the only hard pin
# for the plugin binary is the bootstrap TOOLS_VERSION; fallbackToolsVersion just
# tracks it so dev builds fetch a matching token.
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
# Fails if the glob matches nothing or any rewrite leaves TOOLS_VERSION != value,
# so a moved/renamed bootstrap can't drift out of lockstep with provision.go.
set_bootstrap_tools() {
  local f tmp updated
  local bootstraps=(plugins/*/scripts/bootstrap.sh)
  [[ -e "${bootstraps[0]}" ]] || {
    echo "error: no plugin bootstrap.sh files found" >&2
    exit 1
  }
  for f in "${bootstraps[@]}"; do
    tmp=$(mktemp)
    cp -p "$f" "$tmp" # preserve the executable bit (bootstrap.sh is 0755)
    sed -E "s/^(TOOLS_VERSION=)\"[^\"]+\"/\1\"$1\"/" "$f" >"$tmp"
    updated=$(sed -nE 's/^TOOLS_VERSION="([0-9]+\.[0-9]+\.[0-9]+)".*/\1/p' "$tmp")
    [[ "$updated" == "$1" ]] || {
      rm -f "$tmp"
      echo "error: failed to update TOOLS_VERSION to $1 in $f" >&2
      exit 1
    }
    mv "$tmp" "$f"
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
# The bootstrap TOOLS_VERSION is the real pin (see header); fallbackToolsVersion
# is dev-only and deliberately NOT the currency signal: using it here would
# force a provision.go edit on every tools release, which cuts another tools
# release, which reopens this PR, forever. Sample EVERY bootstrap and use the
# lowest pin, so a partially-bumped tree reads as changed and self-heals (the
# update path rewrites all bootstraps to the same value) instead of hiding
# behind changed=false.
bootstrap_pins=()
for b in plugins/*/scripts/bootstrap.sh; do
  v=$(sed -nE 's/^TOOLS_VERSION="([0-9]+\.[0-9]+\.[0-9]+)".*/\1/p' "$b")
  [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "error: missing or invalid TOOLS_VERSION in $b" >&2
    exit 1
  }
  bootstrap_pins+=("$v")
done
[[ ${#bootstrap_pins[@]} -gt 0 ]] || {
  echo "error: no plugin bootstrap.sh files found" >&2
  exit 1
}
cur_tools=$(printf '%s\n' "${bootstrap_pins[@]}" | sort -V | head -1)

emit server "$new_server"
emit client "$new_client"
emit tools "$new_tools"

if [[ "$new_server" == "$cur_server" && "$new_client" == "$cur_client" && "$new_tools" == "$cur_tools" ]]; then
  emit changed false
  echo "pins already current (server=$cur_server client=$cur_client tools=$cur_tools)"
  exit 0
fi

# Update the Go consts and every bootstrap's TOOLS_VERSION. fallbackToolsVersion
# rides along only when provision.go is already changing for a real pin: it is
# read only by dev builds, and letting it be the sole tools/ change would cut a
# junk tools release and loop this workflow (release -> bump PR -> release).
set_goconst serverVersion "$new_server"
set_goconst clientVersion "$new_client"
if [[ "$new_server" != "$cur_server" || "$new_client" != "$cur_client" ]]; then
  set_goconst fallbackToolsVersion "$new_tools"
fi
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
  _=$(bump_json_version plugins/opencode-memory/package.json)
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
