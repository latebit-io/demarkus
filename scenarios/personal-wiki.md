---
layout: default
title: Personal Knowledge Base
permalink: /scenarios/personal-wiki/
---

# Personal Knowledge Base

Run Demarkus locally as a private markdown wiki — browse with the TUI, edit with your text editor or the CLI.

## What you'll have

- A local server serving your markdown files
- Interactive TUI browser with link navigation
- CLI for quick fetches and edits
- Full version history of every change
- No cloud, no account, no tracking

## Setup

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Then install the server separately (or full install):

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

### 2. Create your content directory

```bash
mkdir -p ~/wiki
echo "# My Wiki\n\n- [Notes](notes.md)" > ~/wiki/index.md
echo "# Notes\n\nStart writing here." > ~/wiki/notes.md
```

### 3. Start the server

```bash
demarkus-server -root ~/wiki
```

The server uses a built-in self-signed certificate and listens on `localhost:6309`.

### 4. Browse with the TUI

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

### 5. Enable writes (optional)

Generate an auth token to allow publishing:

```bash
demarkus-token generate -paths "/*" -ops publish -tokens ~/wiki/tokens.toml
```

Restart the server:

```bash
demarkus-server -root ~/wiki -tokens ~/wiki/tokens.toml
```

### 6. Edit documents

```bash
# Opens in $EDITOR, publishes on save/quit
demarkus edit --insecure -auth <your-token> mark://localhost:6309/notes.md
```

Or publish directly:

```bash
demarkus --insecure -X PUBLISH -auth <your-token> mark://localhost:6309/hello.md \
  -body "# Hello\n\nNew document."
```

## Running on startup (macOS)

If you used the full installer, the server starts automatically via launchd. To start/stop manually:

```bash
# macOS 14+
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.demarkus.server.plist
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/io.demarkus.server.plist

# macOS 13 and earlier
launchctl load ~/Library/LaunchAgents/io.demarkus.server.plist
launchctl unload ~/Library/LaunchAgents/io.demarkus.server.plist
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
