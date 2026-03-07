---
layout: default
title: Agent Install
permalink: /agent-install/
---

# Agent Install

Use this page when you want an AI agent to install and configure Demarkus for you.

## Copy/Paste Brief for Agents

```text
Install and configure Demarkus on this machine.

Goals:
1. Use the official install script:
   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
   - For client-only: add --client-only flag
   - For Let's Encrypt TLS: add --domain yourdomain.com --root /srv/site
   - For custom certs: add --tls-cert /path/cert.pem --tls-key /path/key.pem
   - On Linux, run with sudo for server installs
2. Start demarkus-server and verify client connectivity.
3. If requested, configure MCP using demarkus-mcp and create/update .mcp.json.
4. Report exactly what was installed, where configs were written, and how to run/stop services.

Constraints:
- Do not assume trusted TLS certs are available.
- For self-signed/local certs, use client --insecure where needed.
- Keep commands idempotent and explain any destructive steps before running them.
- If INSTALL_DIR (~/.local/bin or /usr/local/bin) is not in PATH, add it.
```

## Human Inputs to Provide

- Target mode:
  - `public` (with domain + Let's Encrypt)
  - `private/local` (self-signed/dev cert)
- Operating system (`macOS` or `Linux`)
- Desired content root path (for example `/srv/site` or `~/my-docs`)
- Whether MCP should be configured now (`yes/no`)
- If MCP = yes: host + token values

## Minimal Verification Checklist

1. `demarkus-server` starts successfully.
2. CLI fetch works:
   - `demarkus --insecure mark://localhost:6309/index.md` (self-signed/local)
   - or without `--insecure` when using trusted TLS.
3. If MCP configured:
   - `demarkus-mcp` can initialize and `mark_list` works.

## Platform-specific guides

- [macOS](/install/macos/)
- [Linux](/install/linux/)
- [Windows (WSL2)](/install/windows/)

For the agent memory (soul) scenario, see [Agent Memory](/scenarios/agent-memory/).
