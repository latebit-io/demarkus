---
name: knowledge-join
description: Join an organizational demarkus knowledge system (broker-fronted universe) — validates the broker URL, registers it as a Codex MCP server, and points you at OAuth login.
---

Connect this Codex installation to an organizational demarkus knowledge system. The user supplies the public URL of the org's demarkus-broker MCP gateway; this command validates the URL, derives a short server slug from the hostname, and registers it with `codex mcp add` so the broker's 14-tool surface appears as a single MCP server.

This is the broker-fronted counterpart to `/soul-init` (which configures a personal, direct-QUIC soul). A Codex installation can have both — different commands, different MCP server entries, different auth modes.

## Argument

The user invokes this command with the broker URL:

```bash
/knowledge-join https://mcp.broker.acme.com
```

If the user invokes without an argument, ask them for the URL before running anything. Do NOT guess a URL.

## Steps

1. **Validate + derive slug.** Run the helper script:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-join <broker-url>
   ```

   The script does HTTPS validation, fetches the broker's `/.well-known/oauth-protected-resource` (RFC 9728) to confirm it speaks the MCP gateway, and derives a slug from the broker hostname. Output is line-oriented `key=value`.

2. **Read the script output.**

   - If the first line is `OK`, parse the following lines:
     - `url=...`   — canonicalized broker URL (no trailing slash)
     - `slug=...`  — short identifier for the MCP server entry
     - `mcp-url=...` — the `${url}/mcp` endpoint Codex will connect to

     Continue to step 3.

   - If the first line is `FAIL: <message>`, do NOT run `codex mcp add`. Show the user the message verbatim and one suggested next step based on the error:
     - "broker URL must use https://" → ask the user to confirm the URL. If they're testing against a local broker, suggest port-forwarding + opening an issue rather than enabling the env-var escape hatch (which is test-only).
     - "broker unreachable" → ask the user to confirm the URL is reachable from their network (DNS, VPN, corporate proxy).
     - "broker has no /.well-known/oauth-protected-resource (HTTP 404)" → suggest they verify the URL points at the MCP host (typically `mcp.broker.<org>.com`), not the management API host.
     - any other `FAIL` → surface the message as-is.

3. **Register the MCP server, then authenticate.** Register the broker as a streamable-HTTP MCP server (note the `--url` flag — a bare positional would be treated as a stdio command), then run OAuth login:

   ```bash
   codex mcp add <slug> --url <mcp-url>
   codex mcp login <slug>
   ```

   `codex mcp login` runs the OAuth flow against the broker — there is no token to capture or paste here. (Equivalently, add a `[mcp_servers.<slug>]` table with `url = "<mcp-url>"` to `~/.codex/config.toml`, then `codex mcp login <slug>`.)

4. **Record the system for the publish gate.** After `codex mcp add` succeeds, run:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-register <slug>
   ```

   Check the exit status. On success it records the slug so the publish tag-gate enforces tags on writes to this knowledge system, the same way it does for the local soul. (It does not gate unrelated demarkus servers the user may have configured.) **If the script fails (non-zero exit), surface its error to the user and stop here — do not report that the gate is covering this system, because it is not yet.**

5. **Adopt the system's conventions (`root` hub).** A knowledge system can publish org-wide conventions to its guaranteed `root` hub world. Once the OAuth flow is complete and you are doing real work against this system, fetch them and follow them:

   - `mark_fetch mark://root/.well-known/demarkus/template.md` — the required per-world structure (same shape as the local `/project-template.md`). Follow it.
   - `mark_fetch mark://root/.well-known/demarkus/policy.md` — the system's write policy. The gate runs offline (it cannot reach the broker), so its enforced core must be mirrored to local files. **Do not hand-write those files** — pipe the fetched policy **body** to the mirror script, which deterministically parses `strictness:` / `require_tags:` / `require_fields:` and writes (or clears) each per-slug file:

     ```bash
     cat <<'POLICY' | "$HOME/.demarkus/bin/demarkus-plugin" registry policy-mirror <slug>
     <the full body returned by the mark_fetch above>
     POLICY
     ```

     A knob absent from the policy clears its mirror file, so relaxing the policy de-enforces. The mirror is a **snapshot, not a live read** — re-run this on a fresh join whenever the policy changes. At publish time the gate then checks each enforced axis (an `axis:value` tag, e.g. `category:project`) and field (a non-empty key in the `metadata` object, e.g. `metadata: {"type": "Reference"}`).

   If either document is `not-found`, the system simply hasn't declared that convention yet — skip silently. See the demarkus-knowledge plugin's `examples/knowledge-system/` for the format an admin publishes.

6. **Confirm success.** Tell the user, in plain language:

   > Added knowledge system **<slug>** at <url>. Run `codex mcp login <slug>` to complete the OAuth flow against the broker. After that, the org's full demarkus tool surface (mark_fetch, mark_publish, mark_graph, etc.) is available against worlds the broker exposes (URL form: `mark://<worldName>/<path>`).

   Mention that they can list joined knowledge systems with `codex mcp list`. To fully remove this one, both the MCP server **and** the gate registration must go — otherwise the publish gate keeps enforcing tags/axes for a system that's no longer connected:

   ```bash
   codex mcp logout <slug>   # drop stored OAuth credentials first
   codex mcp remove <slug>
   "$HOME/.demarkus/bin/demarkus-plugin" registry knowledge-unregister <slug>
   ```

   (`knowledge-unregister` also clears the mirrored per-slug policy files.)

## Don't

- Don't proceed past step 1 if the script emits `FAIL`. Surface the error and stop. The slug-and-URL output is only valid after `OK`.
- Don't try to obtain or store any tokens locally. The plugin's `~/.demarkus/plugin-memory.token` is for the LOCAL soul (`/soul-init`); knowledge-system auth is handled entirely by Codex's MCP OAuth machinery (`codex mcp login`) against the broker.
- Don't run `/knowledge-join` automatically — only when the user invokes it.
- Don't conflate this command with `/soul-init`. They configure different things:
  - `/soul-init`: personal, direct-QUIC, local demarkus-server, plugin-managed token.
  - `/knowledge-join`: organizational, HTTPS-fronted broker, OAuth-managed token via Codex.
