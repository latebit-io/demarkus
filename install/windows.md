---
layout: default
title: Install on Windows (WSL2)
permalink: /install/windows/
---

# Install on Windows (WSL2)

Demarkus runs natively on Linux. On Windows, use WSL2 (Windows Subsystem for Linux).

## Prerequisites

1. **Enable WSL2** — open PowerShell as Administrator:

```powershell
wsl --install
```

Restart when prompted. This installs Ubuntu by default.

2. **Open a WSL2 terminal** — launch "Ubuntu" from the Start menu or run `wsl` in PowerShell.

## Install inside WSL2

From your WSL2 terminal, follow the [Linux install guide](/install/linux/):

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

For a full server install:

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

## Access from Windows

The server runs inside WSL2 on `localhost:6309` (UDP). Windows can reach it directly — WSL2 bridges the loopback automatically.

From PowerShell or CMD, you can call WSL binaries:

```powershell
wsl demarkus mark://localhost:6309/index.md
```

## Notes

- Systemd in WSL2 requires Ubuntu 22.04+ with the `[boot] systemd=true` option in `/etc/wsl.conf`. If systemd isn't available, start the server manually: `demarkus-server -root ~/my-docs`
- The install script detects WSL2 when run from a Windows shell (Git Bash, MSYS2, etc.) and prints instructions to use WSL2 instead.

## Windows binaries (experimental)

Native Windows binaries (`windows/amd64`, `windows/arm64`) are included in each release on [GitHub Releases](https://github.com/latebit-io/demarkus/releases). These are not yet officially supported — no installer, no service wrapper. Download, extract, and run manually.

## Related

- [Getting Started](/getting-started/)
- [Troubleshooting](/troubleshooting/)
