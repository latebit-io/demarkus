---
layout: default
title: Troubleshooting
permalink: /troubleshooting/
nav: docs
---

# Troubleshooting

## Plugin issues

### Memory tools missing or failing

Run `/soul-status`: it checks the connection and says what to fix. `/soul-init`
reconfigures the local soul. The session status line also shows a ⚠️ warning
when something is unhealthy.

- **Cursor**: if `demarkus-memory` shows as failed before the first conversation, toggle it once under Settings → MCP.
- **OpenCode**: restart after install; the first session provisions the memory.
- **pi**: run `/mcp` once (or restart) to connect the newly registered server.

### Knowledge system won't connect

Re-run `/knowledge-join <url>` with the full `https://` URL and finish sign-in
in the browser. In Cursor, restart and complete OAuth under Settings → MCP; in
OpenCode V2, open `/mcps`, select the server, and sign in.

### A write is refused

The refusal names the fix:

- **Missing tags or axes**: add them to `metadata`, using values from the policy.
- **Secret detected**: remove it; the line is named.
- **Near-duplicate**: update the document it names, or pass `near-duplicate-override` with a reason to keep both.
- **Wrong destination**: the project writes to its bound memory; `/soul-default` changes the binding.

---

## Install issues

### `Permission denied` writing to `/usr/local/bin`

The installer tries sudo when `/usr/local/bin` is not writable, including when
piped from curl. If sudo is unavailable, a fresh install uses `~/.local/bin`.

If you see permission errors, check that `~/.local/bin` is in your PATH:

```bash
echo $PATH | grep -o "$HOME/.local/bin"
```

If not:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

### `command not found: demarkus`

The install directory isn't in PATH. See above.

### Checksum mismatch

The download may be corrupt. Delete the temp files and retry:

```bash
# Just re-run the install script
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

---

## Connection issues

### `x509: certificate signed by unknown authority`

The server is using the built-in self-signed certificate. Add `--insecure`:

```bash
demarkus --insecure mark://localhost:6309/index.md
demarkus-tui --insecure mark://localhost:6309/index.md
```

Only use `--insecure` for local/trusted servers.

### `connection refused` or timeout

The server isn't running. Check:

**macOS (launchd):**

```bash
# Check if the service is loaded
launchctl list | grep demarkus

# View logs
tail -f ~/.demarkus/logs/demarkus.err
```

**Linux (systemd):**

```bash
sudo systemctl status demarkus
journalctl -u demarkus -f
```

**Manual start (any platform):**

```bash
demarkus-server -root ~/my-docs
```

### Can't connect from another machine

Make sure UDP port 6309 is open in your firewall:

```bash
# Linux
sudo ufw allow 6309/udp

# Verify the server is listening
ss -ulnp | grep 6309
```

---

## Server issues

### `No such file or directory` on startup

The content root doesn't exist. Create it:

```bash
mkdir -p /srv/site
demarkus-publish -root /srv/site -path /index.md -body "# Hello"
```

### Writes returning `unauthorized` or `forbidden`

No tokens file is configured, or the token is wrong. Check:

1. Start the server with `-tokens /path/to/tokens.toml`
2. Make sure the token value matches what `demarkus-token generate` printed
3. Use `-auth <raw-token>` in the client (not the token ID)

### Server won't start after `launchctl load` (macOS 14+)

`launchctl load` is deprecated on macOS 14+. Use:

```bash
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/io.latebit.demarkus.plist
```

---

## TUI issues

### TUI shows blank screen

The TUI requires a document path, not just a host:

```bash
# Wrong
demarkus-tui --insecure mark://localhost:6309

# Correct
demarkus-tui --insecure mark://localhost:6309/index.md
```

### Links don't navigate

Use `Tab` to cycle links, then `Enter` to follow. The highlighted link text shows the current selection.

---

## MCP issues

### `demarkus-mcp` not found

The MCP binary is installed alongside `demarkus` and `demarkus-tui`. If it's missing, reinstall:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

### MCP tools not appearing in Claude Code

Check your `.mcp.json` path is in the project root and the `command` path is absolute:

```bash
which demarkus-mcp
# Use that absolute path in .mcp.json
```

### `mark_publish` returns `conflict`

Your `expected_version` is stale. Review the returned merge candidate, or fetch
the latest body and reconcile your changes before retrying at the new version.

---

## Getting help

- [GitHub Issues](https://github.com/latebit-io/demarkus/issues)
- Browse the project memory: `demarkus-tui mark://soul.demarkus.io/index.md`
