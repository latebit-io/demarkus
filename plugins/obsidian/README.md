# Obsidian Plugin

The Obsidian plugin for Demarkus now lives in its own repository:

**→ [latebit-io/obsidian-demarkus](https://github.com/latebit-io/obsidian-demarkus)**

## Install

Install via [BRAT](https://github.com/TfTHacker/obsidian42-brat) by adding
`latebit-io/obsidian-demarkus` as a beta plugin. See the plugin's own README
for full setup.

## How it works

The plugin shells out to the `demarkus` CLI binary, so you still need `demarkus`
installed from this monorepo (`make client` or the [install script](https://demarkus.io/getting-started/)).
Auth tokens are passed through the `DEMARKUS_AUTH` environment variable, never
on the command line.

## Why it moved

Keeping the source in its own repo lets the plugin follow its own release
cadence, use its own issue tracker, and matches the convention the Obsidian
community expects (BRAT installs from a dedicated repo).

See [demarkus.io/ecosystem](https://demarkus.io/ecosystem/) for the full list
of Demarkus-aware clients and integrations.
