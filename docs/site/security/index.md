# Security Model

Demarkus is a versioned markdown document server. This page describes the attack surface, threat model, and hardening options for production deployments.

## Attack Surface

The server is deliberately minimal:

- **No code execution** — serves and stores markdown text, nothing else
- **No shell access** — no CGI, no templates, no scripting
- **No database** — documents are files on disk
- **No sessions or cookies** — stateless request handling
- **Single directory** — all reads and writes are scoped to the content root
- **7 verbs** — FETCH, LIST, VERSIONS, LOOKUP (read), PUBLISH, APPEND, ARCHIVE (write)
- **Encrypted transport** — QUIC with TLS (self-signed for local, real certs for production)
- **Size limits** — 1 MiB body, 64 KB frontmatter per request
- **Rate limiting** — per-IP request throttling built in

## Authentication

Write operations (PUBLISH, APPEND, ARCHIVE) require a capability token — a SHA-256 hash scoped to specific paths and operations.

- Tokens are generated offline with `demarkus-token generate`
- Tokens can be scoped to path patterns (e.g., `/docs/*`) and specific operations
- Every write is logged with the token label for auditing
- Tokens are revocable by removing them from the tokens file and sending SIGHUP
- Read auth is opt-in — configure read tokens for private paths when needed

Without a valid token, write requests are rejected. Without read tokens configured, all content is public (the default for public servers).

## What a Compromised Token Gets You

If an attacker obtains a write token, they can:

- Publish, overwrite, or archive markdown files within the token's path scope
- Nothing else — no code execution, no filesystem escape, no privilege escalation

Mitigation:

- Scope tokens narrowly (e.g., `/blog/*` instead of `/*`)
- Rotate tokens periodically
- Review audit logs for unexpected writes
- Every version is immutable — overwritten content is still in the version history

## What a Compromised Server Gets You

If an attacker gains control of the server process, they can:

- Read and write files in the content directory

With systemd hardening (see below), they **cannot**:

- Access `/home` (`ProtectHome=yes` — omitted automatically if the content root is under `/home`)
- Write to `/etc`, `/usr`, or anywhere outside the content root (mounted read-only)
- Escalate privileges
- Load kernel modules
- See the host `/tmp` (private mount)
- Create setuid/setgid binaries

## Systemd Hardening

For production Linux deployments, add these directives to the systemd unit to sandbox the server process:

```ini
[Service]
ProtectSystem=strict
ReadWritePaths=/srv/site
PrivateTmp=yes
NoNewPrivileges=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
```

`ProtectSystem=strict` makes the entire filesystem read-only. `ReadWritePaths` grants an exception for the content directory only. The kernel enforces these restrictions — no application changes needed.

Set `ReadWritePaths` to match your `DEMARKUS_ROOT`. The server only reads the tokens file and TLS certificates, so paths like `/etc/demarkus` don't need write access — `ProtectSystem=strict` already allows reads.

## Read-Only Mode (Maximum Lockdown)

For Gemini-level security, run the server in read-only mode inside a chroot:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-readonly.sh | sudo bash -s -- --domain example.com
```

This installs the server with `-read-only` flag inside a chroot (`/srv/demarkus`). The process cannot write to the filesystem at all — not even the content directory. Publish content locally with `demarkus-publish`:

```bash
demarkus-publish -root /srv/demarkus/content -path /index.md -body "# Hello"
```

Full versioning is preserved because `demarkus-publish` writes directly to the versioned store on disk. The server just serves what's there.

## External Links in the TUI

The TUI (`demarkus-tui`) can follow links whose scheme is not `mark://` — for example `https://`, `gemini://`, or `mailto:` — by handing the URL to the operating system's default handler (`open` on macOS, `xdg-open` on Linux, `rundll32 url.dll,FileProtocolHandler` on Windows).

The security-relevant properties of this path:

- **Scheme allowlist, narrow by default.** Only `http`, `https`, `gemini`, and `mailto` are opened. Dangerous schemes like `file:`, `javascript:`, `data:`, and custom handlers (`vscode:`, `steam:`, `ms-*:`) are rejected. Configure with `-external-links "http,https"` or disable entirely with `-external-links ""`.
- **URL is passed as an argv argument, never through a shell.** Shell metacharacters in a URL (`;`, `$(…)`, backticks, `|`, newlines) cannot escape into command execution because no shell is ever invoked.
- **Content-served URLs are not fetched automatically.** Following an external link is a deliberate user action (Enter on a selected link or a mouse click) — documents cannot auto-open external URLs.
- **The CLI and MCP server never launch external handlers.** They are non-interactive and must not spawn GUI processes on the user's behalf. Only the TUI does this.

The residual risk is the external handler itself. If your browser, email client, or Gemini reader has a vulnerability that can be triggered by a crafted URL, that vulnerability is reachable via external links in a demarkus document. Keeping the default scheme allowlist narrow limits which handlers a malicious document can reach.

## Comparison

| | SSH | Web + CGI | Gemini | Demarkus |
|---|---|---|---|---|
| Code execution | Shell access | Scripts/templates | None | None |
| Database | Full access | Often | None | None |
| Write scope | Entire machine | Varies | None (read-only) | Content dir only |
| Auth model | Keys / passwords | Sessions / cookies | Client certs | Capability tokens |
| Worst case | Full compromise | RCE, data breach | DoS | Markdown overwrite |

## Related

- [Deployment & TLS](../deployment/index.md)
- [Token Tooling](../tools/index.md)
- [Configuration Reference](../reference/index.md)
