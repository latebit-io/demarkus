---
description: Join a remote demarkus soul (direct-QUIC) — validates the host, stores the token in a 0600 file (not inline in config), registers it as a managed MCP server, and binds it to this project.
---

Connect this pi installation to a **remote demarkus soul** — a
direct-QUIC demarkus-server reached over the network (e.g. `soul.demarkus.io`),
as opposed to the local soul that `/soul-init` manages or the broker-fronted
knowledge system that `/knowledge-join` joins.

This replaces hand-wiring a `demarkus-mcp` entry into the MCP config. The managed
form is strictly better: the auth token lives in a `0600` file and is injected at
launch by `demarkus-plugin mcp-serve`, so it never sits in plaintext in your MCP config.

## Argument

```bash
/soul-join mark://soul.demarkus.io --token <TOKEN> --insecure
```

- The host may be a `mark://host[:port]` URL or a bare host (scheme inferred).
- `--token <TOKEN>` is the capability token for the soul. Omit only for a
  read-only / public soul. The plugin cannot mint a token for a remote server
  (its `tokens.toml` is not local) — the user supplies one.
- `--insecure` skips TLS cert verification (needed for some self-hosted souls;
  `soul.demarkus.io` uses it).

**Token handling — do not put the token in the transcript.** A capability token
is a secret; if it appears in a chat message or a tool-call command line it is
captured in the session transcript. So for any soul that needs a token, you do
NOT run the join yourself — instead give the user the exact command and have them
run it in their own terminal (where the token never reaches you). Never ask the
user to paste the token to you, and never echo it. (A read-only / public soul
needs no token, so you may run that join directly.)

If invoked without a host, ask for it. Do NOT guess. If the user does not say
whether the soul needs a token, ask before proceeding.

## Steps

1. **Validate + register.** Run the helper, passing this project's directory so
   the soul is bound to the repo:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry soul-join <host> [--token <TOKEN>] [--insecure] --bind "${PWD}"
   ```

   **If the soul needs a token, do NOT run this yourself** — print the command
   (with `<TOKEN>` left as a placeholder) and ask the user to run it in their own
   terminal, then paste back only the non-secret `key=value` output lines (slug,
   host, insecure, token-file). For a no-token (public/read-only) soul, you may
   run it directly.

   It normalizes the host, derives a slug from the first DNS label, writes the
   token to `~/.demarkus/soul-<slug>.token` (mode 600), records the soul in the
   catalog (`~/.demarkus/souls`), and binds the project. Output is line-oriented
   `key=value`.

   - On `OK`, parse `slug=`, `host=`, `insecure=`, `token-file=`.
   - On `FAIL: <message>`, do NOT register the MCP server. Show the message verbatim.
     Common cases: an `https://` URL → tell them to use `/knowledge-join`; a
     reserved `demarkus-memory` slug → tell them to join a host with a different
     first label; a slug already joined for a different host.

   Reachability is not probed (demarkus is QUIC — no HTTP metadata to check like
   a broker). The first tool call is the real validation; if it fails, re-join
   (often the fix is adding `--insecure` or a token).

2. **Register the MCP server** (launched via `demarkus-plugin mcp-serve`, which
   injects the token from the 0600 file) in the pi-mcp-adapter config:

   ```bash
   "$HOME/.demarkus/bin/demarkus-plugin" registry mcp add <slug> "$HOME/.demarkus/bin/demarkus-plugin" mcp-serve --soul <slug>
   ```

   (`<slug>` comes from step 1's output.) The token is NOT passed here. After
   registering, the user reconnects MCP servers with `/mcp` (or restarts pi) so
   the new soul's tools load.

3. **Confirm.** Tell the user, in plain language:

   > Joined remote soul **<slug>** at <host>, bound to this project. Its
   > `mark_*` tools appear as the `<slug>` MCP server, with the token injected
   > from `<token-file>` (not stored in your MCP config). The publish tag-gate
   > now enforces tags on writes to it, same as the local soul.

   Mention `"$HOME/.demarkus/bin/demarkus-plugin" registry mcp list` to see it and `"$HOME/.demarkus/bin/demarkus-plugin" registry mcp remove <slug>` to undo.

## Don't

- Don't register the MCP server if step 1 emitted `FAIL`.
- Don't read, echo, or store the token yourself — pass it via the `registry soul-join` command you hand the user,
  which writes it to the 0600 file. It must never land in the transcript.
- Don't use this for an `https://` broker — that is `/knowledge-join`.
- Don't conflate the three soul surfaces:
  - `/soul-init`: local, plugin-managed demarkus-server, plugin-minted token.
  - `/soul-join`: remote, direct-QUIC demarkus-server, user-supplied token.
  - `/knowledge-join`: organizational, HTTPS broker, OAuth via pi.
