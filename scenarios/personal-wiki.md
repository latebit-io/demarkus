---
layout: default
title: Personal Knowledge Base
permalink: /scenarios/personal-wiki/
---

# Personal knowledge base

A private wiki on your machine: write from the CLI, your editor, or your agent;
browse in the TUI; every save versioned. No cloud, no account.

If you run the [memory plugin](/memory/), you already have one: it is your soul.
This guide sets one up by hand.

## Set up

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

The installer starts the server as a service and saves an initial token to
`~/.demarkus/initial-token.txt`. For private reads too, make a token with both
ops and restart the server:

```bash
demarkus-token generate -label my-wiki -paths "/**" -ops read,publish -tokens ~/.demarkus/tokens.toml
export DEMARKUS_AUTH='<raw-token>'
```

macOS paths shown; Linux uses `/var/lib/demarkus` for content and
`/etc/demarkus` for config.

## Write

```bash
demarkus edit --insecure mark://localhost:6309/notes.md     # opens $EDITOR, publishes on save

demarkus --insecure -X PUBLISH -expected-version 0 \
  -body "# My Wiki

- [Notes](notes.md)" mark://localhost:6309/index.md
```

Always write through the server, never to the directory: only published
writes get a version, a hash, and history.

## Read

```bash
demarkus-tui --insecure mark://localhost:6309/index.md      # Tab, Enter, [ ], d for the graph
demarkus --insecure -X VERSIONS mark://localhost:6309/index.md
```

[TUI keys](/reference/tui-keybindings/) · [macOS](/install/macos/) · [Linux](/install/linux/)
