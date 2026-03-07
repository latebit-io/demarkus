---
layout: default
title: Getting Started
permalink: /getting-started/
---

# Getting Started

Get Demarkus running and browse your first document in under 5 minutes.

## 1. Install

**macOS / Linux:**

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

This installs `demarkus`, `demarkus-tui`, and `demarkus-mcp` to `~/.local/bin` (or `/usr/local/bin` if writable).

> Need a server too? See [Install on macOS](/install/macos/) or [Install on Linux](/install/linux/).

## 2. Browse the live soul

The Demarkus project runs its own live server at `mark://soul.demarkus.io`. You can browse it right away:

```bash
# Fetch the index
demarkus mark://soul.demarkus.io/index.md

# Interactive browser
demarkus-tui mark://soul.demarkus.io/index.md
```

**TUI keyboard shortcuts:** `Tab` cycles links, `Enter` follows, `[`/`]` navigate history, `?` for help.

## 3. Run your own server (optional)

Create a content directory and start a server locally:

```bash
mkdir ~/my-docs
echo "# Hello Demarkus" > ~/my-docs/index.md

demarkus-server -root ~/my-docs
```

Then browse it:

```bash
demarkus --insecure mark://localhost:6309/index.md
demarkus-tui --insecure mark://localhost:6309/index.md
```

> Use `--insecure` when connecting to a local server with the built-in self-signed certificate.

## 4. Publish a document

To write to your server, generate an auth token first:

```bash
demarkus-token generate -paths "/*" -ops publish -tokens ~/my-docs/tokens.toml
```

Then restart the server with the tokens file:

```bash
demarkus-server -root ~/my-docs -tokens ~/my-docs/tokens.toml
```

Now publish:

```bash
demarkus --insecure -X PUBLISH -auth <your-token> mark://localhost:6309/hello.md -body "# Hello World"
```

## What's next?

Pick your path:

- [Personal knowledge base](/scenarios/personal-wiki/) — local notes, TUI browser, edit workflow
- [Agent memory](/scenarios/agent-memory/) — persistent memory for Claude Code
- [Public hub](/scenarios/public-hub/) — VPS + TLS + open access
- [Team knowledge base](/scenarios/team/) — shared server with token-based access
