# Rebranding the Claude Code memory plugin

Audience: a team that wants the demarkus memory or knowledge plugin for Claude Code under its own plugin name, generated and published from its own fork.

The demarkus repository ships no brands. A fork declares one in `plugins/prompt-source/manifest.json`, runs the generator, and publishes the resulting directory through its own marketplace. Everything under the brand is generated from the same templates as the upstream plugins, so keeping the fork current is a merge plus a regeneration.

## What a brand changes and what it keeps

A brand changes the plugin name, its description, and every place the prompts name the plugin. It keeps, on purpose:

- the MCP server key `demarkus-memory` in `.mcp.json`. The `demarkus-plugin` binary identifies the local memory by that key; the destination gate, project binding, and slug reservation depend on it. Renaming the key means forking the binary.
- the `demarkus-plugin` binary, its pinned version in `scripts/bootstrap.sh`, and its download source (the demarkus GitHub releases).
- the state directory `~/.demarkus` and the `DEMARKUS_*` environment variables.

Because the state is shared, a brand replaces the upstream plugin on a machine. Installing both doubles every hook and both fight over one managed server. Tell your users to uninstall `demarkus-memory` before installing the brand.

Only Claude Code base targets can be branded. The pi and OpenCode adapters carry their names in TypeScript and install scripts and need a copy step of their own.

## Steps

1. Fork `latebit-io/demarkus` and add the upstream as a remote:

   ```bash
   git clone git@github.com:<org>/demarkus.git && cd demarkus
   git remote add upstream https://github.com/latebit-io/demarkus.git
   ```

2. Declare the brand. Add a `brands` list to `plugins/prompt-source/manifest.json` after `targets`:

   ```json
   "brands": [
     {
       "name": "acme-brain",
       "base": "claude-memory",
       "output": "plugins/brands/acme-brain",
       "plugin_name": "acme-brain",
       "description": "Acme Brain: local, versioned memory for Claude Code, powered by demarkus."
     }
   ]
   ```

   Fields: `base` is `claude-memory` or `claude-knowledge`; `output` must live under `plugins/brands/`; `plugin_name` is lowercase letters, digits, and hyphens and becomes the `/<plugin_name>:` command prefix; `memory_plugin_name` and `knowledge_plugin_name` are optional and name the sibling plugin when you brand both surfaces (two brands, one per base).

3. Generate:

   ```bash
   cd tools && go run ./plugin-prompts write && go run ./plugin-prompts check
   ```

   `write` renders the prompts with your names, copies `hooks/`, `scripts/`, and `.mcp.json` from the base plugin, rewrites the name and description in `.claude-plugin/plugin.json`, and writes a README. `check` fails on any drift between templates and generated files; run it in CI.

4. Publish. Add the brand to a marketplace file at the root of the repository users will add:

   ```json
   {
     "name": "acme",
     "owner": { "name": "acme" },
     "plugins": [
       {
         "name": "acme-brain",
         "source": "./plugins/brands/acme-brain",
         "description": "Acme Brain: local, versioned memory for Claude Code.",
         "version": "0.13.72"
       }
     ]
   }
   ```

   Users then run `/plugin marketplace add <org>/demarkus` and `/plugin install acme-brain@acme`. Keep `version` equal to the base plugin's `version` in `plugins/claude-code/.claude-plugin/plugin.json`; the generated `plugin.json` carries that value.

5. Stay current. When upstream releases a new binary or changes the prompts:

   ```bash
   git fetch upstream && git merge upstream/main
   cd tools && go run ./plugin-prompts write
   git add plugins/brands && git commit
   ```

   Upstream's pin-bump bot updates `plugins/*/scripts/bootstrap.sh`; the merge brings the new pin into the base plugin and `write` copies it into the brand. If your fork runs its own pin-bump automation, include `plugins/brands/*/scripts/bootstrap.sh` in what it stages.

## Customizing beyond the name

The generated directory is a build output; edit the templates under `plugins/prompt-source/`, not the brand. Template text that should differ per brand belongs behind a new manifest field: add it to the `target` struct in `tools/plugin-prompts/main.go`, thread it through `brandTarget` in `brand.go`, and reference it as `{{.Field}}`. That keeps `check` meaningful and merges from upstream clean.
