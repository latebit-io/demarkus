---
description: Detect and configure the demarkus-memory soul (reuse existing or install new)
---

Configure the demarkus-memory plugin's connection to a demarkus-server. Run this once to set up, or again to reconfigure.

## Steps

1. Run detection using the Bash tool:

   `"$HOME/.demarkus/bin/demarkus-plugin" provision detect`

2. Read the output carefully:

   - If the output is exactly `NO_SERVER`, no demarkus-server is running. Run:
     `"$HOME/.demarkus/bin/demarkus-plugin" provision init default`
     Do not assume the install landed at `~/.demarkus/soul/` — `default` falls back to isolated mode on a different root and port when 6310 is taken. Use step 3 to report the actual mode, soul path, and port from the script's stderr or `~/.demarkus/plugin-memory.conf`.

   - If the output starts with `SERVERS`, one or more demarkus-server processes are running. Each following line is `PID PORT ROOT`. Show all of them to the user in a clean list, then ask:

     > Detected running demarkus-server(s). Pick one to reuse for this plugin's memory, or say "isolated" to create a separate instance on its own port:

     Expect the user to either:

     - Pick a row (by PID, port, or "the one at /path") → run:
       `"$HOME/.demarkus/bin/demarkus-plugin" provision init reuse --port <PORT> --root <ROOT>`
       using the values from the chosen row. This appends a plugin token to that server's `tokens.toml` and sends SIGHUP to reload.
     - Say "isolated" or "new" → run:
       `"$HOME/.demarkus/bin/demarkus-plugin" provision init isolated`
       This creates a separate soul at `~/.demarkus/plugin-soul/` on a free port in `16310+`.

3. **Register the MCP server** so its `mark_*` tools are available now (the
   extension also does this on each session start, but registering here makes it
   live without waiting for a restart):

   `"$HOME/.demarkus/bin/demarkus-plugin" registry mcp add demarkus-memory "$HOME/.demarkus/bin/demarkus-plugin" mcp-serve`

   Then tell the user to run `/mcp` (or restart pi) to connect it. Its tools will
   appear as `demarkus_memory_mark_*`.

4. After setup, report back the chosen mode, soul path, and port. Tell the user they can verify with `/soul`.

## Important

- If multiple demarkus-servers are running and the user is ambiguous, ask which one by PID or path before proceeding — do not guess.
- The config lives at `~/.demarkus/plugin-memory.conf`. Rerunning this command overwrites it.
- In `reuse` mode, the plugin does not manage the server's lifecycle — if the user stops that server later, the plugin's memory will be unavailable until they restart it or rerun `/soul-init`.
