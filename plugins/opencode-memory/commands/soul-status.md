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
     printf '%s ' "$b"
     if [ ! -e ~/.demarkus/bin/$b ]; then echo "(not installed)"
     elif [ ! -x ~/.demarkus/bin/$b ]; then echo "(present but not executable)"
     else ~/.demarkus/bin/$b --version 2>&1 | head -1
     fi
   done
   ```

   Note which binaries are present and what versions they report — and keep the
   causes distinct: "(not installed)" means bootstrap never ran or failed (run
   /soul-init); "(present but not executable)" is a permissions problem; a
   present binary whose `--version` errors either predates the flag (upgraded
   on the next session start) or is broken — report the actual error text, do
   not collapse these into one message.

3. **Check the server process.** Read `SOUL_DIR` and `PORT` from the config. Run:

   ```text
   scan=$(ps -axww -o pid=,args= | awk -v pat=" -root ${SOUL_DIR}" -v pp=" -port ${PORT}" '/demarkus-server/ { p = index($0, pat); q = index($0, pp); if (p && q) { c = substr($0, p + length(pat), 1); d = substr($0, q + length(pp), 1); if ((c == "" || c == " ") && (d == "" || d == " ")) print } }') || { echo "(process inspection failed — inspect ps output manually)"; }
   [ -n "${scan}" ] && printf '%s\n' "${scan}" || echo "(no server running at ${SOUL_DIR} on port ${PORT})"
   ```

   (The awk matches the whole `-root <path>` **and** `-port <PORT>` arguments
   with boundary checks, so spaced paths survive, `/tmp/soul` cannot match
   `/tmp/soul-old`, and a same-root server on another port is not mistaken for
   the configured one. The no-server message is printed only after a successful
   scan with zero matches; a failed `ps`/`awk` is surfaced, not converted into
   "no server".)

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
   - Server not running (managed/isolated mode) → "Run /soul-init to restart, or check that demarkus-server is in the bin directory."
   - Server not running (reuse mode) → the plugin does not manage that server's lifecycle; "Restart it with its own service manager or original launch command, or rerun /soul-init to switch modes."
   - Connectivity unauthorized → "Token mismatch — rerun /soul-init."
   - Binaries missing or stale → "Delete ~/.demarkus/bin/ and restart opencode."
   ```

## Don't

- Don't write to the soul or modify config. This command is purely diagnostic.
- Don't assume the server is broken just because connectivity is `not-found` — an empty soul is normal on a fresh install.
- Don't run `/soul-init` automatically. Recommend it and let the user decide.
