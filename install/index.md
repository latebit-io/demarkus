---
layout: default
title: Install
permalink: /install/
redirect_from:
  - /setup/
---

# Install

## Memory for your agent

Start here. The plugin brings its own binaries; nothing else to install.

**Claude Code**

```text
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

**Claude Cowork**: add `latebit-io/demarkus` as a marketplace (Customize →
Plugins), then install `demarkus-memory`.

**Cursor**

```text
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
```

**OpenCode**

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

**pi**

```bash
pi install npm:pi-mcp-adapter
pi install git:github.com/latebit-io/demarkus-pi-memory
```

All plugins share `~/.demarkus`: one memory across every agent on the machine.
[Memory →](/memory/)

## Join a knowledge base

Same marketplaces, the `demarkus-knowledge` plugin, then
`/knowledge-join <url>`. [Knowledge →](/knowledge/#join)

## Run a knowledge system

Worlds, single sign-on, MCP gateways, and the [library](/library/) on
Kubernetes from one Helm values file.

```bash
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --create-namespace --values knowledge-system.yaml
```

[Prerequisites and configuration →](/install/stack/)

## Binaries

Server, CLI, TUI, and MCP server, for self-hosting by hand.

```bash
# macOS / Linux: server + client (Linux: pipe to sudo bash)
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash

# Client only: CLI, TUI, MCP
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

| Platform | Guide |
|---|---|
| macOS | [launchd service, TLS, build from source](/install/macos/) |
| Linux | [systemd, Let's Encrypt, read-only chroot, reading room](/install/linux/) |
| Windows | [inside WSL2](/install/windows/) |
| Docker | [multi-arch image, compose, TLS](/install/docker/) |

[Upgrade & uninstall](/upgrade/) · [Troubleshooting](/troubleshooting/) · [Self-hosting guides](/scenarios/)
