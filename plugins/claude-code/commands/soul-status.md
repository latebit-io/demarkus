---
description: Show the demarkus-memory plugin's connection state and verify it's healthy
---

Diagnose the plugin's current setup: configured mode, soul path, port, server process, installed binaries, and connectivity. Useful when something is acting up or before reporting an issue.

## Steps

1. **Read the plugin config.** Run `cat ~/.demarkus/plugin-memory.conf` via the Bash tool. If it's missing, tell the user the plugin isn't configured yet and suggest `/soul-init`, then stop.

2. **Inspect the binaries.** Run:

   ```
   ls -la ~/.demarkus/bin/ 2>/dev/null
   cat ~/.demarkus/bin/.versions 2>/dev/null || echo "(no version sentinel — pre-tracking install)"
   ```

   Note which binaries are present and what versions they report.

3. **Check the server process.** Read `SOUL_DIR` and `PORT` from the config. Run:

   ```
   pgrep -af "demarkus-server.*-root ${SOUL_DIR}" || echo "(no server running at ${SOUL_DIR})"
   ```

4. **Probe connectivity.** Call `mark_fetch /index.md` via the MCP tool. Note whether it returns `ok`, `not-found`, `unauthorized`, or fails outright.

5. **Render a compact status report.** Use this shape:

   ```
   ## demarkus-memory status

   - **Mode:** <default | isolated | reuse>
   - **Soul:** <SOUL_DIR>
   - **Port:** <PORT>
   - **Server:** running (pid <N>) | not running | reused (user-managed)
   - **Binaries:** demarkus-server <ver>, demarkus-mcp <ver>, demarkus-token <ver>  (or "version sentinel missing")
   - **Connectivity:** ok | not-found (empty soul) | unauthorized (token mismatch) | unreachable

   <If anything is degraded, suggest the specific fix:>
   - Server not running → "Run /soul-init to restart, or check that demarkus-server is in the bin directory."
   - Connectivity unauthorized → "Token mismatch — rerun /soul-init."
   - Binaries missing or stale → "Delete ~/.demarkus/bin/ and trigger SessionStart (reopen Claude Code)."
   ```

## Don't

- Don't write to the soul or modify config. This command is purely diagnostic.
- Don't assume the server is broken just because connectivity is `not-found` — an empty soul is normal on a fresh install.
- Don't run `/soul-init` automatically. Recommend it and let the user decide.
