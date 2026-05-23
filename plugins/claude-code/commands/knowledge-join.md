---
description: Join an organizational demarkus knowledge system (broker-fronted universe) — validates the broker URL, registers it as a Claude Code MCP server, and points you at the device-flow auth on first tool call.
---

Connect this Claude Code installation to an organizational demarkus knowledge system. The user supplies the public URL of the org's demarkus-broker MCP gateway; this command validates the URL, derives a short server slug from the hostname, and registers it with `claude mcp add` so the broker's 13-tool surface appears as a single MCP server.

This is the broker-fronted counterpart to `/soul-init` (which configures a personal, direct-QUIC soul). A Claude Code installation can have both — different commands, different MCP server entries, different auth modes.

## Argument

The user invokes this command with the broker URL:

```
/knowledge-join https://mcp.broker.acme.com
```

If the user invokes without an argument, ask them for the URL before running anything. Do NOT guess a URL.

## Steps

1. **Validate + derive slug.** Run the helper script:

   ```
   bash "${CLAUDE_PLUGIN_ROOT}/scripts/knowledge-join.sh" <broker-url>
   ```

   The script does HTTPS validation, fetches the broker's `/.well-known/oauth-protected-resource` (RFC 9728) to confirm it speaks the MCP gateway, and derives a slug from the broker hostname. Output is line-oriented `key=value`.

2. **Read the script output.**

   - If the first line is `OK`, parse the following lines:
     - `url=...`   — canonicalized broker URL (no trailing slash)
     - `slug=...`  — short identifier for the MCP server entry
     - `mcp-url=...` — the `${url}/mcp` endpoint Claude Code will connect to

     Continue to step 3.

   - If the first line is `FAIL: <message>`, do NOT run `claude mcp add`. Show the user the message verbatim and one suggested next step based on the error:
     - "broker URL must use https://" → ask the user to confirm the URL. If they're testing against a local broker, suggest port-forwarding + opening an issue rather than enabling the env-var escape hatch (which is test-only).
     - "broker unreachable" → ask the user to confirm the URL is reachable from their network (DNS, VPN, corporate proxy).
     - "broker has no /.well-known/oauth-protected-resource (HTTP 404)" → suggest they verify the URL points at the MCP host (typically `mcp.broker.<org>.com`), not the management API host.
     - any other `FAIL` → surface the message as-is.

3. **Register the MCP server.** Run:

   ```
   claude mcp add --transport http <slug> <mcp-url>
   ```

   Claude Code performs the OAuth device flow against the broker on the first tool call — there is no token to capture or paste here.

4. **Confirm success.** Tell the user, in plain language:

   > Added knowledge system **<slug>** at <url>. Claude Code will prompt you to complete the OAuth device flow against the broker the next time it talks to that MCP server. After that, the org's full demarkus tool surface (mark_fetch, mark_publish, mark_graph, etc.) is available against worlds the broker exposes (URL form: `mark://<worldName>/<path>`).

   Mention that they can list joined knowledge systems with `claude mcp list` and remove this one with `claude mcp remove <slug>`.

## Don't

- Don't proceed past step 1 if the script emits `FAIL`. Surface the error and stop. The slug-and-URL output is only valid after `OK`.
- Don't try to obtain or store any tokens locally. The plugin's `~/.demarkus/plugin-memory.token` is for the LOCAL soul (`/soul-init`); knowledge-system auth is handled entirely by Claude Code's MCP OAuth machinery against the broker.
- Don't run `/knowledge-join` automatically — only when the user invokes it.
- Don't conflate this command with `/soul-init`. They configure different things:
  - `/soul-init`: personal, direct-QUIC, local demarkus-server, plugin-managed token.
  - `/knowledge-join`: organizational, HTTPS-fronted broker, OAuth-managed token via Claude Code.
