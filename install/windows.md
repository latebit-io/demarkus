---
layout: default
title: Install on Windows (WSL2)
permalink: /install/windows/
---

# Install on Windows (WSL2)

Demarkus runs natively on Linux. On Windows, install it inside WSL2 (Windows Subsystem for Linux).

## Prerequisites

### 1. Enable WSL2

Open PowerShell as Administrator:

```powershell
wsl --install
```

Restart when prompted. This installs Ubuntu by default. Open "Ubuntu" from the Start menu or run `wsl` in PowerShell.

### 2. Enable systemd (server only)

The server runs as a systemd service. If your WSL distribution has not enabled
systemd, add the setting below. Skip this for client-only installs.

Inside your WSL2 terminal:

```bash
sudo tee -a /etc/wsl.conf >/dev/null <<'EOF'

[boot]
systemd=true
EOF
```

Then from a Windows PowerShell:

```powershell
wsl --shutdown
```

Re-open WSL. Verify systemd is running:

```bash
systemctl is-system-running
```

## Install inside WSL2

### Client only

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash -s -- --client-only
```

### Full server + client

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | sudo bash
```

## Access from Windows

Demarkus uses **QUIC over UDP**. Run its clients inside WSL2:

Any client running inside WSL2 reaches the server directly at `mark://localhost:6309`:

```bash
demarkus --insecure mark://localhost:6309/health
```

From PowerShell or CMD, invoke the WSL binary:

```powershell
wsl demarkus --insecure mark://localhost:6309/health
```

Windows `netsh interface portproxy` forwards TCP, not UDP. For access from other
machines, configure [WSL mirrored networking](https://learn.microsoft.com/en-us/windows/wsl/networking#mirrored-mode-networking)
and its firewall, or deploy the server on a Linux host.

## Related

- [Linux install](/install/linux/)
- [Troubleshooting](/troubleshooting/)
