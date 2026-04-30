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

The server runs as a systemd service. WSL2 supports systemd, but it's opt-in. Skip this step if you're only installing the client.

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

WSL2 auto-forwards **TCP** to the Windows host, but Demarkus uses **QUIC over UDP** (port 6309), which is not forwarded automatically. You have two options.

### Option A — Connect from inside WSL2 (simplest)

Any client running inside WSL2 reaches the server directly at `mark://localhost:6309`:

```bash
demarkus mark://localhost:6309/index.md
```

From PowerShell or CMD, invoke the WSL binary:

```powershell
wsl demarkus mark://localhost:6309/index.md
```

### Option B — Forward UDP 6309 to Windows

Find your WSL2 IP from inside WSL:

```bash
ip addr show eth0 | grep 'inet '
```

Then from an **Administrator** PowerShell (replace `172.x.x.x` with your WSL2 IP):

```powershell
netsh interface portproxy add v4tov4 listenport=6309 listenaddress=0.0.0.0 `
  connectport=6309 connectaddress=172.x.x.x protocol=udp

New-NetFirewallRule -DisplayName "Demarkus QUIC" -Direction Inbound `
  -Action Allow -Protocol UDP -LocalPort 6309
```

WSL2's virtual IP changes on each reboot. For a stable external address, either script the `portproxy` re-registration at boot or run a reverse proxy outside WSL2.

## Related

- [Linux install](/install/linux/)
- [Troubleshooting](/troubleshooting/)
