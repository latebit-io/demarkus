---
description: Join a remote demarkus soul (direct-QUIC) — validates the host, stores the token in a 0600 file (not inline in config), registers it as a managed MCP server, and binds it to this project.
---

Connect this Claude Code installation to a **remote demarkus soul** — a
direct-QUIC demarkus-server reached over the network (e.g. `soul.demarkus.io`),
as opposed to the local soul that `/soul-init` manages or the broker-fronted
knowledge system that `/knowledge-join` joins.

This replaces hand-wiring a `demarkus-mcp` entry into `.mcp.json`. The managed
form is strictly better: the auth token lives in a `0600` file and is injected at
launch via a wrapper, so it never sits in plaintext in `.mcp.json` or in
`claude mcp list` output.

## Argument

```bash
# Join URL (preferred): one string carries the host and token.
# Emitted by install.sh and demarkus-token join.
/soul-join mark://kb.example.com#token=<RAW> [--insecure]

# Explicit form
/soul-join mark://soul.demarkus.io --token <TOKEN> --insecure
```

- **Join URL**: the `#token=` fragment supplies the capability token; the
  helper extracts and stores it.
- Otherwise the host may be a `mark://host[:port]` URL or a bare host.
- `--token <TOKEN>` is the capability token for the soul. Omit only for a
  read-only / public soul. The plugin cannot mint a token for a remote server
  (its `tokens.toml` is not local) — the user supplies one. Do not combine
  with a join URL that already carries a token.
- `--insecure` skips TLS cert verification (needed for self-signed souls with
  either form; `soul.demarkus.io` uses it).

If invoked without a host, ask for it. Do NOT guess. If the user does not give a
join URL or token, ask whether the soul needs one before proceeding.

## Steps

1. **Check for hand-wired souls to adopt.** Run:

   ```bash
   bash "${CLAUDE_PLUGIN_ROOT}/scripts/detect-manual-souls.sh"
   ```

   - If the output is `NONE`, continue to step 2.
   - If it starts with `MANUAL`, each following line is
     `<name>\t<host>\t<insecure>\t<has-token>`. Show the user the unmanaged
     entries and offer to adopt one: re-join it under managed config. To adopt,
     use that row's host/insecure as the `/soul-join` arguments below, ask the
     user for the token (it is in their existing entry — never read or echo it
     yourself), and after a successful join run `claude mcp remove <name>` to
     drop the old hand-wired entry. If they decline, continue.

2. **Validate + register.** Run the helper, passing this project's directory so
   the soul is bound to the repo:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry soul-join '<host-or-join-url>' [--token <TOKEN>] [--insecure] --bind "${CLAUDE_PROJECT_DIR}"
   ```

   Quote the argument: a join URL's `#fragment` is shell-significant. The
   helper normalizes the host (extracting the token from a join URL), derives
   a slug from the first DNS label, writes the token to
   `~/.demarkus/soul-<slug>.token` (mode 600), records the soul in the
   catalog (`~/.demarkus/souls`), and binds the project. Output is
   line-oriented `key=value`.

   - On `OK`, parse `slug=`, `host=`, `insecure=`, `token-file=`.
   - On `FAIL: <message>`, do NOT run `claude mcp add`. Show the message verbatim.
     Common cases: an `https://` URL → tell them to use `/knowledge-join`; a
     reserved `demarkus-memory` slug → tell them to join a host with a different
     first label; a slug already joined for a different host.

   Reachability is not probed (demarkus is QUIC — no HTTP metadata to check like
   a broker). The first tool call is the real validation; if it fails, re-join
   (often the fix is adding `--insecure` or a token).

3. **Register the MCP server** (launched via `demarkus-plugin mcp-serve`, which
   injects the token from the 0600 file). Default to project scope so the soul
   travels with this repo; use `--scope user` for every project:

   ```bash
   claude mcp add <slug> --scope project -- "$HOME/.demarkus/bin/demarkus-plugin" mcp-serve --soul <slug>
   ```

   (`<slug>` comes from step 2's output.) The token is NOT passed here.

4. **Confirm.** Tell the user, in plain language:

   > Joined remote soul **<slug>** at <host>, bound to this project. Its
   > `mark_*` tools appear as the `<slug>` MCP server, with the token injected
   > from `<token-file>` (not stored in your MCP config). The publish tag-gate
   > now enforces tags on writes to it, same as the local soul.

   Mention `claude mcp list` to see it and `claude mcp remove <slug>` to undo.

## Don't

- Don't run `claude mcp add` if step 2 emitted `FAIL`.
- Don't read, echo, or store the token yourself — pass the join URL or token
  through to the helper, which writes it to the 0600 file. A pasted join URL
  is already in the transcript, but never repeat its fragment back in output.
- Don't use this for an `https://` broker — that is `/knowledge-join`.
- Don't conflate the three soul surfaces:
  - `/soul-init`: local, plugin-managed demarkus-server, plugin-minted token.
  - `/soul-join`: remote, direct-QUIC demarkus-server, user-supplied token.
  - `/knowledge-join`: organizational, HTTPS broker, OAuth via Claude Code.
