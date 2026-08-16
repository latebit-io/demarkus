---
description: Join an organizational demarkus knowledge system through its HTTPS broker and OpenCode OAuth.
---

Connect this OpenCode installation to an organizational demarkus knowledge system. This joins an HTTPS broker, not a direct-QUIC soul. OpenCode owns OAuth credentials; this command never requests or stores them.

## Argument

The user supplies the broker base URL:

```bash
/knowledge-join https://knowledge.acme.com
```

If no URL was supplied, ask for it. Do not guess.

## Join

1. Before invoking Bash, require the entire argument to match `https://` plus a DNS hostname, optional numeric port, and optional trailing slash only. Reject whitespace, shell metacharacters, credentials, paths, queries, and fragments rather than trying to escape them.

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-join '<broker-url>'
   ```

   Continue only when the command exits zero, the first line is exactly `OK`, and exactly one nonempty `url=`, `slug=`, and `mcp-url=` line follows. Reject unexpected output, a slug outside `[a-z0-9][a-z0-9-]*`, a non-HTTPS URL, or an MCP URL other than `<url>/mcp`. Reject `demarkus-memory` and names ending in `-demarkus-memory`; those are local-soul names.

2. Check all three registration views before changing state:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry mcp list
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-list
   opencode mcp list
   ```

   If a command fails or `<slug>` already exists in any view, stop. Do not overwrite or remove existing state. Ask for explicit removal first when the user wants to replace it.

3. Register the shared endpoint, then the publish-gate entry:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry mcp add-http '<slug>' '<mcp-url>'
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-register '<slug>'
   ```

   Check each result before running the next command. Each registry mutation is internally locked and atomic. If endpoint registration fails, stop without running `knowledge-register`. If knowledge registration fails, leave the successfully registered endpoint in place, report that exact partial state, and provide `registry knowledge-register '<slug>'` as the repair after its error is corrected. Do not perform speculative rollback or remove state that another harness may use.

4. After both commands succeed, report that local registration is complete and this invocation is finished. Tell the user to restart OpenCode, complete OAuth when prompted, or run `opencode mcp auth '<slug>'`. Do not wait for OAuth, invoke MCP tools, or continue the current command across the restart.

## After Restart

In a later session, require `opencode mcp list` to show the exact slug and verify access with the read-only `<slug>_mark_worlds` tool. Then fetch the system's root conventions through `<slug>_mark_fetch` with `force=true`:

- `mark://root/.well-known/demarkus/template.md`
- `mark://root/.well-known/demarkus/policy.md`

Accept a document only when the result is `status: ok`, contains the full body, and is not `mode: outline`. Follow existing documents. If policy exists, create a temporary file with `mktemp`; require a safe absolute path, a regular file, and mode `0600`. Write the exact body with the Write tool, read it back, and require a byte-for-byte match before passing it to `registry policy-mirror '<slug>'` through stdin. Remove it afterward and surface cleanup failure. Never embed broker content in shell source or arguments. If policy is `not-found`, pass empty stdin to clear stale mirrors. Report failures and do not claim policy enforcement updated unless mirroring succeeds.

## Remove

Removal is a separate, user-requested operation. Require the entire slug to match `[a-z0-9][a-z0-9-]*`; reject reserved local-soul names. Before changing state, explicitly tell the user that the endpoint catalog is shared and removing this row will also disconnect pi. Abort unless the user confirms that impact.

After confirmation, run each command only after the previous one succeeds:

```bash
opencode mcp logout '<slug>'
"$HOME/.demarkus/bin/demarkus-plugin" registry mcp remove '<slug>'
"$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-unregister '<slug>'
```

If a command fails, stop, list the remaining endpoint and gate state, and report the repair command. After all three succeed, tell the user to restart OpenCode; do not keep this invocation alive across that restart. A manually configured `config.mcp[slug]` entry must be removed through the mechanism that created it.

## Don't

- Do not continue after validation failure or a slug collision.
- Do not request, read, paste, or store OAuth tokens.
- Do not use this command for `mark://` direct souls; use `/soul-join`.
- Do not run this command automatically.
