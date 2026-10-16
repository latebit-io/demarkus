---
layout: default
title: Install
permalink: /install/
---

# Install

Pick your platform, or deploy a shared knowledge system.

## Knowledge system (Kubernetes)

GCS-backed worlds, OIDC gateways, and an optional [reading room](/library/), configured through one Helm values file.

```bash
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --create-namespace --values knowledge-system.yaml
```

[Prerequisites and configuration →](/install/stack/)

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

[Full Linux guide →](/install/linux/), including Let's Encrypt TLS and the read-only chroot install.

## Windows (WSL2)

Demarkus runs inside WSL2. Same install script as Linux, run from your WSL2 shell.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | sudo bash
```

[Full Windows guide →](/install/windows/), covering WSL2 setup and systemd enablement.

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

[Full Docker guide →](/install/docker/): TLS, docker-compose, environment config.

## Client only (no server)

Skip the service setup and just install the CLI, TUI, and MCP binaries. Works on macOS, Linux, and WSL2:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

Use this if you only want to browse remote servers like `mark://soul.demarkus.io`.
