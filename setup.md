---
layout: default
title: Setup
permalink: /setup/
---

# Setup

Demarkus can be run in different ways depending on whether you need personal notes, team docs, public documentation, or agent integrations.

## Quick Install Script (`install.sh`)

Use the installer from the `main` branch to bootstrap binaries and defaults:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

Common options:

```bash
# Install with Let's Encrypt + custom content root
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain docs.example.com --root /srv/site

# Client-only install
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --client-only

# No official cert (self-signed/dev cert flow)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --no-tls
```

Then continue with one of the setup patterns below, depending on your use case.

Need an AI agent to do this for you? Use the [Agent Install brief](/agent-install/).

## macOS Recommendation (Build From Source First)

Until official signed binaries are in place, macOS users should prefer building from source:

```bash
make client
make server
```

Then run directly:

```bash
./server/bin/demarkus-server -root ./content -port 6309
./client/bin/demarkus --insecure mark://localhost:6309/index.md
```

## 1. Local Solo Setup

Best for personal notes, testing, and offline-first workflows.

```bash
make client
make server
./server/bin/demarkus-server -root ./content -port 6309
```

Use the CLI:

```bash
./client/bin/demarkus mark://localhost:6309/index.md
```

## 2. Public Docs Server

Best for publishing public protocol docs, guides, and knowledge bases.

- Run `demarkus-server` on a host with TLS configured
- Serve a markdown content root
- Share `mark://` URLs for direct retrieval
- Optionally mirror content for resilience

### TLS with Let's Encrypt

The install script on `main` can provision Let's Encrypt automatically when you pass `--domain`.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain docs.example.com --root /srv/site
```

Manual fallback (Certbot + explicit cert paths):

```bash
sudo certbot certonly --standalone -d docs.example.com
./server/bin/demarkus-server \
  -root ./content \
  -port 6309 \
  -tls-cert /etc/letsencrypt/live/docs.example.com/fullchain.pem \
  -tls-key /etc/letsencrypt/live/docs.example.com/privkey.pem
```

For renewal, use:

```bash
sudo certbot renew --quiet --deploy-hook "systemctl restart demarkus"
```

### No Official Certificate (Self-Signed)

If you do not want or cannot use an official certificate yet, Demarkus can still run with a self-signed/dev certificate.

- The installer supports this path directly (`--no-tls`), and local setups can run immediately.
- For clients, use `--insecure` when connecting to self-signed endpoints.

```bash
./client/bin/demarkus --insecure mark://localhost:6309/index.md
./client/bin/demarkus-tui --insecure mark://localhost:6309/index.md
```

## 3. Private Team Knowledge Base

Best for internal runbooks, architecture docs, and decision logs.

- Use capability tokens for scoped access
- Keep version history and audit trail enabled
- Restrict publishing/archiving tokens to trusted operators
- Expose read-only paths for broader team visibility

## 4. demarkus-soul (Agent Memory)

Best for persistent memory between AI sessions.

- Run a dedicated Demarkus server (separate content root)
- Connect `demarkus-mcp` to that host
- Store architecture notes, debugging logs, roadmap, journal, and thoughts
- Use MCP tools (`mark_fetch`, `mark_list`, `mark_versions`) for retrieval and evolution

### MCP Setup (`demarkus-soul`)

1. Start the soul server:

```bash
./server/bin/demarkus-server -root ./soul-root -port 6310
```

2. Build the MCP binary:

```bash
make client
```

3. Configure your MCP client with `.mcp.json`:

```json
{
  "mcpServers": {
    "demarkus-soul": {
      "command": "/absolute/path/to/client/bin/demarkus-mcp",
      "args": [
        "-host", "mark://localhost:6310",
        "-token", "<your-token>",
        "-insecure"
      ]
    }
  }
}
```

Use `-insecure` for local/self-signed development. For remote production hosts, use trusted TLS and remove `-insecure`.

## 5. Hybrid Public + Private

Best when you need open docs and restricted internal docs together.

- Use separate roots/hosts for public and private spaces
- Keep public docs mirrorable and indexable
- Keep private docs token-gated
- Reuse the same markdown + versioning workflow across both
