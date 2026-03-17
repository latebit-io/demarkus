# Security Model

Demarkus is a versioned markdown document server. This page describes the attack surface, threat model, and hardening options for production deployments.

## Attack Surface

The server is deliberately minimal:

- **No code execution** — serves and stores markdown text, nothing else
- **No shell access** — no CGI, no templates, no scripting
- **No database** — documents are files on disk
- **No sessions or cookies** — stateless request handling
- **Single directory** — all reads and writes are scoped to the content root
- **6 verbs** — FETCH, LIST, VERSIONS (read), PUBLISH, APPEND, ARCHIVE (write)
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

- Read `/home`, `/etc`, or anything outside the content root
- Escalate privileges
- Load kernel modules
- Access `/tmp` (private mount)
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

Set `ReadWritePaths` to match your `DEMARKUS_ROOT`. If your tokens file is outside the content root, add its path too:

```ini
ReadWritePaths=/srv/site /etc/demarkus
```

## Write Isolation (Optional)

For maximum lockdown, separate read and write access:

1. Bind the server to `127.0.0.1` — only local clients can connect
2. Publish content locally (CI/CD, scripts, or the `demarkus` CLI on the same machine)
3. Use iptables to redirect public traffic on UDP 443 to the server port:

```bash
sudo iptables -t nat -A PREROUTING -p udp --dport 443 -j REDIRECT --to-port 6309
```

This gives you the same trust boundary as a static file server — local writes, public reads — while keeping full protocol versioning.

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
