#!/usr/bin/env bash
#
# Resolve the latest released server/client/tools versions and update the
# demarkus-memory plugin's binary pins (and its patch version) to match.
#
# Idempotent: run it as often as you like. It writes "changed=true" plus the
# resolved versions to $GITHUB_OUTPUT (and stdout) when it edited files, and
# "changed=false" when everything was already current. Requires `gh` and `jq`.
#
# Env: GH_REPO (default latebit-io/demarkus), GH_TOKEN for `gh`.
set -euo pipefail

repo="${GH_REPO:-latebit-io/demarkus}"
lib="plugins/claude-code/scripts/lib.sh"
plugin_json="plugins/claude-code/.claude-plugin/plugin.json"
marketplace=".claude-plugin/marketplace.json"
# The marketplace entry this plugin owns, keyed by its source path.
plugin_source="./plugins/claude-code"

# latest <module> -> "X.Y.Z" (highest semver among that module's release tags).
latest() {
  gh release list --repo "$repo" --limit 200 \
    | grep -oE "$1/v[0-9]+\.[0-9]+\.[0-9]+" \
    | sed "s#$1/v##" | sort -V | tail -1
}

# pin <CONST> -> current value of `readonly CONST="..."` in lib.sh.
pin() { sed -nE "s/^readonly $1=\"([^\"]+)\".*/\1/p" "$lib"; }

emit() {
  echo "$1=$2"
  [[ -n "${GITHUB_OUTPUT:-}" ]] && echo "$1=$2" >>"$GITHUB_OUTPUT"
  return 0
}

new_server=$(latest server)
new_client=$(latest client)
new_tools=$(latest tools)
if [[ -z "$new_server" || -z "$new_client" || -z "$new_tools" ]]; then
  echo "error: could not resolve latest server/client/tools versions" >&2
  exit 1
fi

cur_server=$(pin SERVER_VERSION)
cur_client=$(pin CLIENT_VERSION)
cur_tools=$(pin TOOLS_VERSION)

emit server "$new_server"
emit client "$new_client"
emit tools "$new_tools"

if [[ "$new_server" == "$cur_server" && "$new_client" == "$cur_client" && "$new_tools" == "$cur_tools" ]]; then
  emit changed false
  echo "pins already current (server=$cur_server client=$cur_client tools=$cur_tools)"
  exit 0
fi

# Update the three pins. Write via a temp file rather than `sed -i`, whose flag
# syntax differs between GNU and BSD sed — this stays portable.
tmp=$(mktemp)
sed -E \
  -e "s/^(readonly SERVER_VERSION=)\"[^\"]+\"/\1\"$new_server\"/" \
  -e "s/^(readonly CLIENT_VERSION=)\"[^\"]+\"/\1\"$new_client\"/" \
  -e "s/^(readonly TOOLS_VERSION=)\"[^\"]+\"/\1\"$new_tools\"/" \
  "$lib" >"$tmp" && mv "$tmp" "$lib"

# Patch-bump the plugin version, kept in lockstep between plugin.json and the
# marketplace entry. jq targets the exact entry by source path, so a coincident
# version on another plugin is never touched.
cur_ver=$(jq -r '.version' "$plugin_json")
new_ver=$(awk -F. '{printf "%d.%d.%d", $1, $2, $3 + 1}' <<<"$cur_ver")

# plugin.json round-trips cleanly through jq (targets .version exactly).
tmp=$(mktemp)
jq --arg v "$new_ver" '.version = $v' "$plugin_json" >"$tmp" && mv "$tmp" "$plugin_json"

# marketplace.json is edited with awk to preserve its hand-authored formatting
# (jq would expand the inline tag arrays). Anchor on this plugin's exact source
# path (literal substring match), then replace the next version line in that
# entry only — never another plugin's.
tmp=$(mktemp)
awk -v v="$new_ver" -v src="$plugin_source" '
  index($0, "\"source\": \"" src "\"") { inblock = 1 }
  inblock && /"version":/ {
    sub(/"version": "[^"]*"/, "\"version\": \"" v "\"")
    inblock = 0
  }
  { print }
' "$marketplace" >"$tmp" && mv "$tmp" "$marketplace"

emit version "$new_ver"
emit changed true
echo "bumped: server $cur_server->$new_server, client $cur_client->$new_client, tools $cur_tools->$new_tools, plugin $cur_ver->$new_ver"
