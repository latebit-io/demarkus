---
layout: default
title: Personal Knowledge Base
permalink: /scenarios/personal-wiki/
---

# Personal Knowledge Base

Run Demarkus locally as a private markdown wiki — browse with the TUI, publish through the protocol, full version history on every document.

## What you'll have

- A local server with version history on every document
- Write access controlled by capability tokens (not filesystem access)
- Interactive TUI browser with link navigation
- CLI for fetches, edits, and publishing
- No cloud, no account, no tracking

## Setup

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

This installs the server, client tools, and registers the server as a background service.

### 2. Create your content directory

```bash
mkdir -p ~/wiki
```

The directory starts empty — you'll populate it through the protocol in the next steps.

### 3. Generate a write token

All writes go through the Mark Protocol. Generate a token to allow publishing:

```bash
demarkus-token generate -paths "/*" -ops publish -tokens ~/wiki/tokens.toml
```

Copy the raw token from the output — you'll use it as `$TOKEN` below.

### 4. Start the server with write access

```bash
demarkus-server -root ~/wiki -tokens ~/wiki/tokens.toml
```

The server uses a built-in self-signed certificate and listens on `localhost:6309`.

> If you used the full installer, edit the launchd/systemd service config to add `-tokens ~/wiki/tokens.toml` and restart.

### 5. Publish your first documents

Everything goes through the protocol — no writing files directly to disk:

```bash
export TOKEN=<your-token>

demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/index.md \
  -body "# My Wiki

- [Notes](notes.md)
- [Ideas](ideas.md)"

demarkus --insecure -X PUBLISH -auth $TOKEN mark://localhost:6309/notes.md \
  -body "# Notes

Start writing here."
```

Each publish creates a new immutable version. Run `demarkus -X VERSIONS` to see the history:

```bash
demarkus --insecure -X VERSIONS mark://localhost:6309/index.md
```

### 6. Browse with the TUI

```bash
demarkus-tui --insecure mark://localhost:6309/index.md
```

**Keyboard shortcuts:**

| Key | Action |
|-----|--------|
| `Tab` | Cycle through links |
| `Enter` | Follow selected link |
| `[` | Back |
| `]` | Forward |
| `d` | Document graph view |
| `?` | Help |

### 7. Edit documents

The `edit` subcommand opens a document in `$EDITOR` and publishes on save:

```bash
demarkus edit --insecure -auth $TOKEN mark://localhost:6309/notes.md
```

If the document doesn't exist yet, it opens an empty editor and creates it on save.

## Why publish through the protocol?

Writing files directly to `~/wiki` bypasses version history — those changes won't be tracked. Everything published via `demarkus -X PUBLISH` gets a version number, a hash, and an immutable record. That's what makes it a knowledge base rather than just a folder of files.

## Running on startup (macOS)

If you used the full installer, the server starts automatically via launchd. To start/stop manually:

```bash
# macOS 14+
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.demarkus.server.plist
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/io.demarkus.server.plist
```

## Running on startup (Linux)

```bash
sudo systemctl enable --now demarkus
sudo systemctl status demarkus
```

## Related

- [Getting Started](/getting-started/)
- [Install on macOS](/install/macos/)
- [Install on Linux](/install/linux/)
