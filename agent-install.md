---
layout: default
title: Agent Install
permalink: /agent-install/
nav: docs
---

# Agent install

Want memory for your agent? Install the [memory plugin](/memory/#install); it
sets itself up. This page is for having an agent install a **server** for you.

## Brief to paste

```text
Install and configure a Demarkus server on this machine.

1. Use the official install script:
   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
   - Client only: --client-only
   - Let's Encrypt: --domain <domain> --root /srv/site
   - Own certs: --tls-cert <cert.pem> --tls-key <key.pem>
   - Linux only, read-only web reading room: --with-library
   - On Linux, pipe to sudo bash for server installs
2. Start demarkus-server and verify: demarkus [--insecure] mark://localhost:6309/health
3. If asked, generate a publish token with demarkus-token and configure demarkus-mcp in .mcp.json.
4. Report what was installed, where configs went, and how to start and stop the service.

Constraints:
- Use --insecure only for self-signed or local certs.
- Keep commands idempotent; explain destructive steps before running them.
- If the install dir (/usr/local/bin or ~/.local/bin) is not on PATH, add it.
```

## Tell it

- Mode: public (domain + Let's Encrypt) or local (self-signed)
- OS: macOS or Linux (Windows: WSL2)
- Content root, e.g. `/srv/site` or `~/my-docs`
- Configure MCP now? If yes, the host and token

## Check

1. `demarkus-server` is running.
2. `demarkus --insecure mark://localhost:6309/health` answers (drop `--insecure` with trusted TLS).
3. With MCP: `mark_list` works.

Guides: [macOS](/install/macos/) · [Linux](/install/linux/) · [Windows](/install/windows/) · [Docker](/install/docker/)
