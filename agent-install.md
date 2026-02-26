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
1. If on macOS, prefer building from source first (until signed binaries are available).
2. Otherwise, use the official install script from:
   https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh
3. Support both:
   - Let's Encrypt mode (if a domain is provided)
   - self-signed/no-official-cert mode (if no domain is provided)
4. Start demarkus-server and verify client connectivity.
5. If requested, configure MCP using demarkus-mcp and create/update .mcp.json.
6. Report exactly what was installed, where configs were written, and how to run/stop services.

Constraints:
- Do not assume trusted TLS certs are available.
- For self-signed/local certs, use client --insecure where needed.
- Keep commands idempotent and explain any destructive steps before running them.
```

## Human Inputs to Provide

- Target mode:
  - `public` (with domain + Let's Encrypt)
  - `private/local` (self-signed/dev cert)
- Operating system (`macOS` or `Linux`)
- Desired content root path (for example `/srv/site` or `./content`)
- Whether MCP should be configured now (`yes/no`)
- If MCP = yes: host + token values

## Minimal Verification Checklist

1. `demarkus-server` starts successfully.
2. CLI fetch works:
   - `./client/bin/demarkus --insecure mark://localhost:6309/index.md` (self-signed/local)
   - or without `--insecure` when using trusted TLS.
3. If MCP configured:
   - `demarkus-mcp` can initialize and `mark_list` works.

For manual setup paths and examples, see [Setup options](/setup/).
