---
description: Show the demarkus-memory plugin's connection state and verify it's healthy
---

# /soul-status

Diagnose the plugin's current setup: configured mode, soul path, port, server process, installed binaries, and connectivity. Useful when something is acting up or before reporting an issue.

## Steps

1. **Read the plugin config.** Run `cat ~/.demarkus/plugin-memory.conf` via the Bash tool. If it's missing, tell the user the plugin isn't configured yet and suggest `/soul-init`, then stop.

2. **Inspect the binaries.** Run:

   ```text
   ls -la ~/.demarkus/bin/ 2>/dev/null
   for b in demarkus-server demarkus-mcp demarkus-token; do
     printf '%s ' "$b"; ~/.demarkus/bin/$b --version 2>/dev/null || echo "(missing or too old for --version)"
   done
   ```

   Note which binaries are present and what versions they report. A binary that
   prints "(missing or too old for --version)" predates the `--version` flag and
   will be upgraded on the next session start.

3. **Check the server process.** Read `SOUL_DIR` and `PORT` from the config. Run:

   ```text
   ps -axww -o pid=,args= | grep demarkus-server | grep -F -- "-root ${SOUL_DIR}" | grep -v grep || echo "(no server running at ${SOUL_DIR})"
   ```

   (`grep -F` matches `SOUL_DIR` literally, so regex metacharacters or spaces in
   the path can't mis-detect the server.)

4. **Probe connectivity.** Call `mark_fetch /index.md` via the MCP tool. Note whether it returns `ok`, `not-found`, `unauthorized`, or fails outright.

5. **Render a compact status report.** Use this shape:

   ```text
   ## demarkus-memory status

   - **Mode:** <default | isolated | reuse>
   - **Soul:** <SOUL_DIR>
   - **Port:** <PORT>
   - **Server:** running (pid <N>) | not running | reused (user-managed)
   - **Binaries:** demarkus-server <ver>, demarkus-mcp <ver>, demarkus-token <ver>  (or "too old for --version — upgrades next session")
   - **Connectivity:** ok | not-found (empty soul) | unauthorized (token mismatch) | unreachable

   <If anything is degraded, suggest the specific fix:>
   - Server not running → "Run /soul-init to restart, or check that demarkus-server is in the bin directory."
   - Connectivity unauthorized → "Token mismatch — rerun /soul-init."
   - Binaries missing or stale → "Delete ~/.demarkus/bin/ and trigger SessionStart (reopen pi)."
   ```

## Don't

- Don't write to the soul or modify config. This command is purely diagnostic.
- Don't assume the server is broken just because connectivity is `not-found` — an empty soul is normal on a fresh install.
- Don't run `/soul-init` automatically. Recommend it and let the user decide.
