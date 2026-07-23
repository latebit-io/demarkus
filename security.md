---
layout: default
title: Security Model
permalink: /security/
---

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
- **Logs go to stderr** — captured by systemd journal, the server writes nothing outside the content directory

## Authentication

Write operations (PUBLISH, APPEND, ARCHIVE) require a capability token — a SHA-256 hash scoped to specific paths and to an access class (read or write).

- Tokens are generated offline with `demarkus-token generate`
- Tokens can be scoped to path patterns (e.g., `/docs/*`) and to an access class. The write capability is a single `publish` op that grants PUBLISH, APPEND, and ARCHIVE together — these are not individually grantable. Reads are a separate `read` op, only enforced on paths where a read token exists.
- Every write is logged with the token label for auditing
- Tokens are revocable by removing them from the tokens file and sending SIGHUP
- Read auth is opt-in — configure read tokens for private paths when needed

Without a valid token, write requests are rejected. Without read tokens configured, all content is public (the default for public servers).

## What a Compromised Token Gets You

If an attacker obtains a write token, they can:

- Publish, overwrite, or archive markdown files within the token's path scope
- Prune version history on a document they can write, by setting `retention` on a publish
- Nothing else — no code execution, no filesystem escape, no privilege escalation

Retention is the store's only destructive operation. It needs no extra capability — it runs under the same write authorization that could archive the document — so it is constrained instead: deletion targets come from enumerating the document's own stored versions, never from request input; every removal resolves inside an `os.Root` anchored at the store root, so a planted symlink cannot redirect it; and each pruned range is audit-logged with the writer's token label. Clients warn before setting `retention` on a document that is not a generated artifact.

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

For production Linux deployments, the install script automatically adds these directives to the systemd unit:

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

Verify hardening is active:

```bash
systemctl show demarkus -p ProtectSystem,ProtectHome,NoNewPrivileges,ReadWritePaths
```

### Existing Installs

Running `demarkus-install update` detects if the systemd unit is missing hardening and prompts you to apply it. If the service fails to start after hardening, it automatically rolls back to the previous config.

## Read-Only Mode (Maximum Lockdown)

For Gemini-level security, run the server in read-only mode. All PUBLISH, APPEND, and ARCHIVE requests are rejected — the server needs zero write access to the filesystem.

### Quick setup

```bash
demarkus-server -read-only -root /srv/site
```

Or via environment variable:

```bash
DEMARKUS_READ_ONLY=1 demarkus-server -root /srv/site
```

### Chroot install

For maximum isolation, use the read-only install script. It runs the server inside a chroot with a fully read-only filesystem:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-readonly.sh | sudo bash -s -- --domain example.com
```

The chroot structure:

```
/srv/demarkus/
  bin/demarkus-server    <- binary inside chroot
  content/               <- documents
  tls/cert.pem           <- certificates
  tls/key.pem
```

The systemd unit uses `RootDirectory` and `ReadOnlyPaths=/` — the process cannot see or write anything outside the chroot.

### Publishing content locally

Publish with `demarkus-publish` — it writes directly to the versioned store on disk, bypassing the server:

```bash
demarkus-publish -root /srv/demarkus/content -path /index.md -body "# Hello"
echo "# Hello" | demarkus-publish -root /srv/demarkus/content -path /index.md
```

Full versioning is preserved — `demarkus-publish` uses the same store code as the server. The server just serves what's on disk.

## Knowledge system (broker)

In a [knowledge system](/scenarios/knowledge-system/) the broker is the only public surface and worlds stay private behind it.

- Users never hold a world token. They authenticate against your IdP; the broker mints a scoped token per world and proxies the request.
- Auth follows the MCP authorization spec: discovery (RFC 8414 / 9728), dynamic client registration (RFC 7591), then authorization code with PKCE.
- The [library](/library/) in broker mode is a confidential web client: tokens stay server-side, the browser holds an opaque session cookie, and state-changing requests carry CSRF protection.
- The AI librarian works only the read ports the reader has, so it cannot surface a document the reader could not fetch.
- Compromising the broker exposes what the worlds behind it serve to authenticated callers; it does not grant write capability beyond the tokens it can mint.

## Comparison

| | SSH | Web + CGI | Gemini | Demarkus |
|---|---|---|---|---|
| Code execution | Shell access | Scripts/templates | None | None |
| Database | Full access | Often | None | None |
| Write scope | Entire machine | Varies | None (read-only) | Content dir only |
| Auth model | Keys / passwords | Sessions / cookies | Client certs | Capability tokens |
| Worst case | Full compromise | RCE, data breach | DoS | Markdown overwrite |

## Related

- [Install on Linux](/install/linux/)
- [Publish a Public Hub](/scenarios/public-hub/)
- [Organizational knowledge system](/scenarios/knowledge-system/)
