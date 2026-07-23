---
layout: default
title: Install
permalink: /install/
---

# Install

Pick your platform — or take the whole stack in one command.

## Whole system (Linux host)

World server, broker, [reading room](/library/), self-hosted login, HTTPS, and the indexing agent, wired together. No DNS setup required.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | sudo bash
```

[Full appliance guide →](/install/stack/)

## macOS

One-line install. Sets up launchd service for the server, plus the CLI, TUI, and MCP binaries.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

[Full macOS guide →](/install/macos/)

## Linux

One-line install. Sets up a systemd service plus the server and client binaries (CLI, TUI, MCP).

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | sudo bash
```

[Full Linux guide →](/install/linux/) — includes Let's Encrypt TLS and read-only chroot install.

## Windows (WSL2)

Demarkus runs inside WSL2. Same install script as Linux, run from your WSL2 shell.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | sudo bash
```

[Full Windows guide →](/install/windows/) — covers WSL2 setup and systemd enablement.

## Docker

Multi-arch image (`linux/amd64`, `linux/arm64`, `linux/arm/v7`) published to GHCR on every release.

```bash
docker run -d \
  --name demarkus \
  -p 6309:6309/udp \
  -v /srv/site:/data \
  ghcr.io/latebit-io/demarkus-server:latest \
  -root /data
```

[Full Docker guide →](/install/docker/) — TLS, docker-compose, environment config.

## OpenClaw

Skill on ClawHub that gives your OpenClaw agent persistent memory over the Mark Protocol.

```bash
clawhub install demarkus
```

[Full OpenClaw guide →](/install/openclaw/)

## Client only (no server)

Skip the service setup and just install the CLI, TUI, and MCP binaries. Works on macOS, Linux, and WSL2:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Use this if you only want to browse remote servers like `mark://soul.demarkus.io`.
