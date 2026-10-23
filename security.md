---
layout: default
title: Security Model
permalink: /security/
nav: docs
---

# Security Model

Memory and knowledge live in servers that store versioned markdown and nothing else. Attack surface, threat model, and hardening below.

## Attack Surface

The server is deliberately minimal:

- **No code execution**: serves and stores markdown text, nothing else
- **No shell access**: no CGI, no templates, no scripting
- **Versioned storage**: local files, or per-world GCS buckets in a knowledge system
- **No protocol cookies**: OIDC gateways and the library manage login separately
- **Scoped storage**: reads and writes stay within the selected world
- **8 verbs**: FETCH, LIST, VERSIONS, LOOKUP, WATCH (read), PUBLISH, APPEND, ARCHIVE (write)
- **Encrypted transport**: QUIC with TLS (self-signed for local, real certs for production)
- **Size limits**: 1 MiB body, 64 KB frontmatter per request
- **Rate limiting**: per-IP request throttling built in
- **Logs go to stderr**: captured by systemd journal, the server writes nothing outside the content directory

## Authentication

Standalone-server writes (PUBLISH, APPEND, ARCHIVE) require a capability token: a random
secret stored server-side as a SHA-256 hash, scoped to paths and operations.

- Tokens are generated offline with `demarkus-token generate`
- Tokens scope paths: `/docs/*` matches one segment, `/docs/**` includes descendants. The `publish` op grants PUBLISH, APPEND, and ARCHIVE together. The separate `read` op gates reads on its paths.
- Every write is logged with the token label for auditing
- Tokens are revocable by removing them from the tokens file and sending SIGHUP
- Read auth is opt-in: configure read tokens for private paths when needed

Without a valid token, write requests are rejected. Without read tokens configured, all content is public (the default for public servers).

## What a Compromised Token Gets You

If an attacker obtains a write token, they can:

- Publish, overwrite, or archive markdown files within the token's path scope
- Prune version history on a document they can write, by setting `retention` on a publish
- Nothing else: no code execution, no filesystem escape, no privilege escalation

Retention is the store's only destructive operation. Deletion targets come from
the document's own stored versions, never from request input; each pruned range
is audit-logged with the writer's label.

Mitigation:

- Scope tokens narrowly (e.g., `/blog/**` instead of `/**`)
- Rotate tokens periodically
- Review audit logs for unexpected writes
- Versions are immutable; older content remains unless explicitly pruned

## What a Compromised Server Gets You

If an attacker gains control of the server process, they can:

- Read and write files in the content directory

With systemd hardening (see below), they **cannot**:

- Access `/home` (`ProtectHome=yes`: omitted automatically if the content root is under `/home`)
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

`ProtectSystem=strict` makes the entire filesystem read-only. `ReadWritePaths` grants an exception for the content directory only. The kernel enforces these restrictions, with no application changes needed.

Set `ReadWritePaths` to match your `DEMARKUS_ROOT`. The server only reads the tokens file and TLS certificates, so paths like `/etc/demarkus` don't need write access; `ProtectSystem=strict` already allows reads.

Verify hardening is active:

```bash
systemctl show demarkus -p ProtectSystem,ProtectHome,NoNewPrivileges,ReadWritePaths
```

### Existing Installs

Running `demarkus-install update` detects if the systemd unit is missing hardening and prompts you to apply it. If the service fails to start after hardening, it automatically rolls back to the previous config.

## Read-Only Mode (Maximum Lockdown)

For Gemini-level security, run the server in read-only mode. All PUBLISH, APPEND, and ARCHIVE requests are rejected, and the server needs zero write access to the filesystem.

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

The systemd unit uses `RootDirectory` and `ReadOnlyPaths=/`, so the process cannot see or write anything outside the chroot.

### Publishing content locally

Publish with `demarkus-publish`. It writes directly to the versioned store on disk, bypassing the server:

```bash
demarkus-publish -root /srv/demarkus/content -path /index.md -body "# Hello"
echo "# Hello" | demarkus-publish -root /srv/demarkus/content -path /index.md
```

Full versioning is preserved: `demarkus-publish` uses the same store code as the server. The server just serves what's on disk.

## Content checks

Every write, to a soul or a knowledge system, passes the same checks at the store.

- **Secrets** are refused, naming the rule and line, never the value. No override.
- **Personal data** is refused by default in a knowledge system and warned in a soul. A reviewed false positive lands with `content-check-override: <reason>`.
- **Near-duplicates** are refused, naming the existing document. `near-duplicate-override: <reason>` keeps both.

## Knowledge system

OIDC gateways front the worlds. Users sign in with your identity provider; no
world tokens are minted or copied.

- **Standard MCP auth**: discovery (RFC 8414, 9728), dynamic client registration (RFC 7591), authorization code with PKCE, resource-bound tokens (RFC 8707), rotating refresh tokens.
- **Per-world access**: reads need the world's `allow`; writes also land only in its `writeScope` paths.
- **Hosted memory**: each identity maps to exactly one world. Reads and writes are locked to it; cross-tenant access is denied at the tool, resource, and crawl layers.
- **Library**: a confidential web client. Tokens stay server-side; the browser holds an opaque session cookie; state-changing requests carry CSRF protection. Its agent reads only what the reader can.
- **Blast radius**: a compromised knowledge process reaches the worlds and buckets its service identity can.

## Comparison

| | SSH | Web + CGI | Gemini | Demarkus |
|---|---|---|---|---|
| Code execution | Shell access | Scripts/templates | None | None |
| Database | Full access | Often | None | None |
| Write scope | Entire machine | Varies | None (read-only) | Content dir only |
| Auth model | Keys / passwords | Sessions / cookies | Client certs | Capability tokens |
| Write-token compromise | Full access within account | Application-dependent | Not applicable | Scoped content changes and history pruning |

## Related

- [Install on Linux](/install/linux/)
- [Publish a Public Hub](/scenarios/public-hub/)
- [Organizational knowledge system](/knowledge/)
