# Rebranding the memory and knowledge plugins

Audience: a team that wants the demarkus memory or knowledge plugin for Claude Code or Cursor under its own plugin name, generated on its own machines or CI and published from its own marketplace.

The demarkus repository ships no brands. A brand is declared in a small `brands.json` that lives in your repository; the generator (`tools/plugin-prompts`) renders it against an unmodified demarkus checkout at a commit you pin. Staying current is a pin bump plus a regeneration, with no fork and no merge.

## What a brand changes and what it keeps

A brand changes the plugin name, its description, and every place the prompts name the plugin. It keeps, on purpose:

- the MCP server key `demarkus-memory` in `.mcp.json` (Claude Code) and `mcp.json` (Cursor). The `demarkus-plugin` binary identifies the local memory by that key; the destination gate, project binding, and slug reservation depend on it. Renaming the key means forking the binary.
- the `demarkus-plugin` binary, its pinned version in `scripts/bootstrap.sh`, and its download source (the demarkus GitHub releases).
- the state directory `~/.demarkus` and the `DEMARKUS_*` environment variables.

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
      "description": "Acme Brain: local, versioned memory for Claude Code, powered by demarkus."
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

Fields: `name` and `output` are unique per entry; `output` must live under `plugins/brands/` of the demarkus checkout; `plugin_name` is lowercase letters, digits, and hyphens, becomes the `/<plugin_name>:` command prefix, and may repeat across harnesses since each harness has its own marketplace; `memory_plugin_name` and `knowledge_plugin_name` name the sibling plugin on the same harness when you brand both surfaces; `store_noun` and `store_noun_plural` replace the word "soul" in the prompts. Unknown fields are rejected.

Rendering:

```bash
git clone https://github.com/latebit-io/demarkus && cd demarkus && git checkout <pinned-commit>
cd tools && go run ./plugin-prompts write --brands /path/to/brands.json \
         && go run ./plugin-prompts check --brands /path/to/brands.json
```

`write` renders the prompts with your names, copies `hooks/` and `scripts/` plus the MCP config of a memory base from the base plugin, rewrites the name and description in the plugin manifest (`.claude-plugin/plugin.json` or `.cursor-plugin/plugin.json`; version and hooks untouched), and writes a README into each `output`. A base missing any of those files fails the render. `check` fails on any drift between templates and generated files. Both need the full checkout, since the copied files come from the base plugin.

## Publishing from your own repository

Layout in the repository your users will add as a marketplace (a monorepo works; the marketplace files must sit at the repository root):

```text
.claude-plugin/marketplace.json    # Claude Code brands
.cursor-plugin/marketplace.json    # Cursor brands
demarkus-plugins/brands.json
demarkus-plugins/upstream.ref      # one demarkus commit SHA
demarkus-plugins/acme-brain/       # generated, committed through a PR
demarkus-plugins/acme-knowledge/
demarkus-plugins/acme-brain-cursor/
demarkus-plugins/acme-knowledge-cursor/
```

Marketplace entry, one per brand in the marketplace file of its harness; `source` paths are relative to the repository root and `version` must equal the base plugin's version, which the generated manifest carries:

```json
{
  "name": "acme",
  "owner": { "name": "acme" },
  "plugins": [
    {
      "name": "acme-brain",
      "source": "./demarkus-plugins/acme-brain",
      "description": "Acme Brain: local, versioned memory for Claude Code.",
      "version": "0.13.131"
    }
  ]
}
```

### Regenerate locally and open a pull request

No CI is required; Go is the only tool. Keep a demarkus clone next to your repository and run this for every update:

```bash
# once
git clone https://github.com/latebit-io/demarkus ~/src/demarkus

# each update: pin, render, copy, PR
cd ~/src/demarkus && git fetch origin && git checkout "$(cat ~/src/acme/demarkus-plugins/upstream.ref)"
(cd tools && go run ./plugin-prompts write --brands ~/src/acme/demarkus-plugins/brands.json)
cd ~/src/acme && git switch -c chore/demarkus-brands
for dir in ~/src/demarkus/plugins/brands/*/; do
  name=$(basename "$dir")
  rm -rf "demarkus-plugins/$name" && cp -R "$dir" "demarkus-plugins/$name"
done
# set each marketplace version to the one in demarkus-plugins/<name>/.claude-plugin/plugin.json or .cursor-plugin/plugin.json
git add demarkus-plugins .claude-plugin .cursor-plugin && git commit -m "chore(plugins): regenerate demarkus brands"
gh pr create --fill
```

Wrap it in a `make brands` target so an update is one command. Bump `upstream.ref` first when you want a newer demarkus.

### Regenerate in CI

A job that regenerates on every change to `brands.json` or `upstream.ref` and opens a pull request:

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
      - run: |
          git clone https://github.com/latebit-io/demarkus /tmp/demarkus
          git -C /tmp/demarkus checkout "$(cat demarkus-plugins/upstream.ref)"
          brands="$GITHUB_WORKSPACE/demarkus-plugins/brands.json"
          (cd /tmp/demarkus/tools && go run ./plugin-prompts write --brands "$brands" && go run ./plugin-prompts check --brands "$brands")
          for dir in /tmp/demarkus/plugins/brands/*/; do
            name=$(basename "$dir")
            rm -rf "demarkus-plugins/$name" && cp -R "$dir" "demarkus-plugins/$name"
            for harness in claude cursor; do
              manifest="demarkus-plugins/$name/.$harness-plugin/plugin.json"
              [ -f "$manifest" ] || continue
              version=$(jq -r .version "$manifest")
              jq --arg n "$(jq -r .name "$manifest")" --arg v "$version" '(.plugins[] | select(.name == $n) | .version) = $v' \
                ".$harness-plugin/marketplace.json" > /tmp/m.json && mv /tmp/m.json ".$harness-plugin/marketplace.json"
            done
          done
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
