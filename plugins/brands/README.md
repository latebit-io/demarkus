# Rebranding the memory and knowledge plugins

Audience: a team that wants the demarkus memory or knowledge plugin for Claude Code or Cursor under its own plugin name, generated on its own machines or CI and published from its own marketplace.

The demarkus repository ships no brands. A brand is declared in a small `brands.json` that lives in your repository; the generator (`tools/plugin-prompts`) renders it against an unmodified demarkus checkout at a commit you pin. Staying current is a pin bump plus a regeneration, with no fork and no merge.

## What a brand changes and what it keeps

A brand changes the plugin name, its description, and every place the prompts name the plugin. It keeps, on purpose:

- the `demarkus-plugin` binary, its pinned version in `scripts/bootstrap.sh`, and its download source (the demarkus GitHub releases).
- the state directory `~/.demarkus` and the `DEMARKUS_*` environment variables.

The memory MCP server key defaults to `demarkus-memory`, which is what tool names and the `plugin:<name>:demarkus-memory` label show. `mcp_server_key` on a memory brand renames it (`plugin:<name>:memory`, tools `mcp__plugin_<name>_memory__mark_*`, prompts under `/memory/`): the generator rewrites `.mcp.json` (Claude Code) or `mcp.json` (Cursor) and adds `mcp-serve --name <key>`, which records the key under `~/.demarkus` so the destination gate and nudges still recognize the local memory. The key is reserved like `demarkus-memory`: no joined store may use it as a slug. Needs a `demarkus-plugin` release with `mcp-serve --name`; pin `upstream.ref` at a pin bump after that change.

Because the state is shared, a brand replaces the upstream plugin on a machine. Installing both doubles every hook and both fight over one managed server. Tell your users to uninstall `demarkus-memory` and `demarkus-knowledge` before installing the brand.

Brandable bases: `claude-memory`, `claude-knowledge`, `cursor-memory`, `cursor-knowledge`. The pi and OpenCode plugins carry their names in TypeScript and install scripts and cannot be branded by the generator.

## The brands file

One entry per plugin you want generated; delete the entries you do not need. This example brands all four:

```json
{
  "brands": [
    {
      "name": "acme-brain",
      "base": "claude-memory",
      "output": "plugins/brands/acme-brain",
      "plugin_name": "acme-brain",
      "knowledge_plugin_name": "acme-knowledge",
      "mcp_server_key": "memory",
      "description": "Acme Brain: local, versioned memory for Claude Code, powered by demarkus.",
      "author": { "name": "Acme", "url": "https://github.com/acme" },
      "homepage": "https://github.com/acme/plugins",
      "repository": "https://github.com/acme/plugins"
    },
    {
      "name": "acme-knowledge",
      "base": "claude-knowledge",
      "output": "plugins/brands/acme-knowledge",
      "plugin_name": "acme-knowledge",
      "memory_plugin_name": "acme-brain",
      "description": "Acme Knowledge: the shared Acme catalog for Claude Code, powered by demarkus."
    },
    {
      "name": "acme-brain-cursor",
      "base": "cursor-memory",
      "output": "plugins/brands/acme-brain-cursor",
      "plugin_name": "acme-brain",
      "knowledge_plugin_name": "acme-knowledge",
      "description": "Acme Brain: local, versioned memory for Cursor, powered by demarkus."
    },
    {
      "name": "acme-knowledge-cursor",
      "base": "cursor-knowledge",
      "output": "plugins/brands/acme-knowledge-cursor",
      "plugin_name": "acme-knowledge",
      "memory_plugin_name": "acme-brain",
      "description": "Acme Knowledge: the shared Acme catalog for Cursor, powered by demarkus."
    }
  ]
}
```

Fields: `name` and `output` are unique per entry; `output` must live under `plugins/brands/` of the demarkus checkout; `plugin_name` is lowercase letters, digits, and hyphens, becomes the `/<plugin_name>:` command prefix, and may repeat across harnesses since each harness has its own marketplace; `memory_plugin_name` and `knowledge_plugin_name` name the sibling plugin on the same harness when you brand both surfaces; `store_noun` and `store_noun_plural` replace the word "soul" in the prompts; `mcp_server_key` (memory bases only) renames the MCP server as described above; `author` (`name` required, `url` optional), `homepage` and `repository` replace the manifest's identity fields, which otherwise keep the demarkus values (URLs must be absolute http(s)). Unknown fields are rejected.

Rendering:

```bash
git clone https://github.com/latebit-io/demarkus && cd demarkus && git checkout <pinned-commit>
cd tools && go run ./plugin-prompts write --brands /path/to/brands.json \
         && go run ./plugin-prompts check --brands /path/to/brands.json
```

`write` renders the prompts with your names, copies `hooks/` and `scripts/` plus the MCP config of a memory base from the base plugin, rewrites the name, description and any set identity fields in the plugin manifest (`.claude-plugin/plugin.json` or `.cursor-plugin/plugin.json`; version and hooks untouched), and writes a README into each `output`. A base missing any of those files fails the render. Every directory under `plugins/brands/` is managed: `write` deletes files and directories no configured brand produces, including the whole directory of a brand removed from the file, and `check` reports them as drift. Both need the full checkout, since the copied files come from the base plugin.

## Publishing from your own repository

Layout in the repository your users will add as a marketplace (a monorepo works; the marketplace files must sit at the repository root):

```text
.claude-plugin/marketplace.json    # Claude Code brands
.cursor-plugin/marketplace.json    # Cursor brands
demarkus-plugins/brands.json
demarkus-plugins/upstream.ref      # one demarkus commit SHA
demarkus-plugins/regen.sh          # the script below
demarkus-plugins/acme-brain/       # generated, committed through a PR
demarkus-plugins/acme-knowledge/
demarkus-plugins/acme-brain-cursor/
demarkus-plugins/acme-knowledge-cursor/
```

Marketplace files, one per harness, hold the repository metadata; `demarkus-plugins/regen.sh` fills their `plugins` arrays from the generated manifests on every run (name, `source`, description, `version`), so a brand removed from `brands.json` disappears from its marketplace and a version bump lands without hand edits. Start each file as:

```json
{ "name": "acme", "owner": { "name": "acme" }, "plugins": [] }
```

The script, `demarkus-plugins/regen.sh`; Go and jq are the only tools:

```bash
#!/usr/bin/env bash
# Render the brands against the pinned demarkus commit, replace the generated
# plugin directories, and rebuild every marketplace entry that points at them.
set -euo pipefail
shopt -s nullglob
here=$(cd "$(dirname "$0")" && pwd) && repo=$(dirname "$here")
up=${DEMARKUS_SRC:-$HOME/src/demarkus}
mkdir -p "$(dirname "$up")"
[ -d "$up/.git" ] || git clone https://github.com/latebit-io/demarkus "$up"
git -C "$up" fetch -q origin && git -C "$up" checkout -q "$(cat "$here/upstream.ref")"
(cd "$up/tools" && go run ./plugin-prompts write --brands "$here/brands.json" && go run ./plugin-prompts check --brands "$here/brands.json")
cd "$repo"
# preflight: every generated harness needs its marketplace file before anything is deleted
for manifest in "$up"/plugins/brands/*/.claude-plugin/plugin.json "$up"/plugins/brands/*/.cursor-plugin/plugin.json; do
  m="$(basename "$(dirname "$manifest")")/marketplace.json"
  [ -f "$m" ] || { echo "regen: $m missing, needed by $manifest" >&2; exit 1; }
done
find demarkus-plugins -mindepth 1 -maxdepth 1 -type d -exec rm -rf {} +
for m in .claude-plugin/marketplace.json .cursor-plugin/marketplace.json; do
  [ -f "$m" ] || continue
  # source may be an object (Cursor); only string sources under demarkus-plugins/ are ours
  jq '.plugins |= map(select(((.source | strings | startswith("./demarkus-plugins/")) // false) | not))' "$m" > "$m.tmp"
  mv "$m.tmp" "$m"
done
for dir in "$up"/plugins/brands/*/; do
  name=$(basename "$dir")
  cp -R "$dir" "demarkus-plugins/$name"
  for manifest in "demarkus-plugins/$name"/.claude-plugin/plugin.json "demarkus-plugins/$name"/.cursor-plugin/plugin.json; do
    [ -f "$manifest" ] || continue
    m="$(basename "$(dirname "$manifest")")/marketplace.json"
    jq --arg src "./demarkus-plugins/$name" --slurpfile p "$manifest" \
      '.plugins += [{name: $p[0].name, source: $src, description: $p[0].description, version: $p[0].version}]' \
      "$m" > "$m.tmp"
    mv "$m.tmp" "$m"
  done
done
```

### Regenerate locally and open a pull request

No CI is required. Bump `upstream.ref` when you want a newer demarkus, then:

```bash
git switch -c chore/demarkus-brands
bash demarkus-plugins/regen.sh
git add demarkus-plugins .claude-plugin .cursor-plugin
git commit -m "chore(plugins): regenerate demarkus brands"
gh pr create --fill
```

A `make brands` target wrapping the script makes an update one command.

### Regenerate in CI

The same script, run by a job that opens a pull request on every change to `brands.json` or `upstream.ref`:

```yaml
name: demarkus brands
on:
  push:
    branches: [main]
    paths: [demarkus-plugins/brands.json, demarkus-plugins/upstream.ref]
  workflow_dispatch:
permissions: { contents: write, pull-requests: write }
jobs:
  generate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - run: DEMARKUS_SRC=/tmp/demarkus bash demarkus-plugins/regen.sh
      - uses: peter-evans/create-pull-request@v6
        with:
          branch: chore/demarkus-brands
          title: "chore(plugins): regenerate demarkus brands"
          add-paths: |
            demarkus-plugins/*
            .claude-plugin/marketplace.json
            .cursor-plugin/marketplace.json
```

Notes:

- Both paths produce the same output; start local, add the job when manual bumps get forgotten.
- Pin `upstream.ref` to a commit made by the upstream pin-bump bot (`chore(plugins): bump managed binaries to latest release`), so the prompts match the binary the plugin bootstraps.
- Pull requests opened with the default `GITHUB_TOKEN` do not trigger other workflows; use a GitHub App token or a PAT if the PR must run your CI.
- The generated directories are build output. Do not hand-edit them; the next regeneration overwrites them. Change the brands file, or the templates upstream.
- Claude Code users run `/plugin marketplace add <org>/<repo>` and `/plugin install acme-brain@acme`. Cursor users import the repository as a marketplace from the plugins panel. A private repository needs git access for every user, since the marketplace add clones it.

## Alternative: brands in a fork

A fork can instead add the same entries as a `brands` list in `plugins/prompt-source/manifest.json` after `targets`, run `go run ./plugin-prompts write` with no flag, commit `plugins/brands/`, and publish through marketplace files in the fork. Staying current is then `git fetch upstream && git merge upstream/main` followed by a regeneration. Prefer the brands file: it needs no merge and keeps the fork's `manifest.json` identical to upstream.

## Customizing beyond the name

The generated directory is a build output; edit the templates under `plugins/prompt-source/`, not the brand. Template text that should differ per brand belongs behind a new brand field: add it to the `brand` struct in `tools/plugin-prompts/brand.go`, thread it through `brandTarget`, and reference it as `{{.Field}}`. That keeps `check` meaningful and every brand renderable from an unmodified checkout.
