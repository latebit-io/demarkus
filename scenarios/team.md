---
layout: default
title: Team Knowledge Base
permalink: /scenarios/team/
---

# Team Knowledge Base

Run a shared Demarkus server for a team — version-controlled docs, per-person or per-role write tokens, and open internal read access.

## What you'll have

- A shared server (VPS or internal host)
- Read access for the whole team — no auth required
- Write access controlled by capability tokens
- Full version history for accountability and rollback
- Everyone uses `demarkus-tui` or `demarkus` CLI to read and write

## Architecture

```
Team server (mark://docs.internal.example.com)
├── /runbooks/          — ops runbooks
├── /decisions/         — architecture decision records
├── /onboarding/        — getting started guides
└── /projects/<name>/   — per-project docs
```

Each team member gets a personal publish token scoped to paths they own. A team lead token covers everything.

## Setup

### 1. Install the server

On a Linux host (VPS or internal):

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --domain docs.internal.example.com --root /srv/team-docs
```

Or with your own certificates:

```bash
sudo curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --tls-cert /path/cert.pem --tls-key /path/key.pem
```

### 2. Create initial structure

```bash
sudo mkdir -p /srv/team-docs/{runbooks,decisions,onboarding}
echo "# Team Docs" | sudo tee /srv/team-docs/index.md
echo "# Runbooks" | sudo tee /srv/team-docs/runbooks/index.md
```

### 3. Generate tokens

**Team lead token (full access):**

```bash
sudo demarkus-token generate \
  -paths "/*" \
  -ops publish,archive \
  -tokens /etc/demarkus/tokens.toml \
  -label "team-lead"
```

**Developer token (scoped to their project folder):**

```bash
sudo demarkus-token generate \
  -paths "/projects/myproject/*" \
  -ops publish \
  -tokens /etc/demarkus/tokens.toml \
  -label "dev-alice"
```

**Read-only token (if you want to restrict reads too):**

Omit `-ops` — by default the server is read-open. To restrict reads, use the token without publish ops.

After generating tokens, reload:

```bash
sudo systemctl restart demarkus
```

### 4. Team members install client

Each team member runs:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --client-only
```

### 5. Team members browse and publish

Browse:

```bash
demarkus-tui mark://docs.internal.example.com/index.md
```

Edit a document (opens `$EDITOR`, publishes on save):

```bash
demarkus edit -auth <token> mark://docs.internal.example.com/runbooks/deploy.md
```

Publish directly:

```bash
demarkus -X PUBLISH -auth <token> \
  mark://docs.internal.example.com/decisions/001-use-demarkus.md \
  -body "# ADR 001: Use Demarkus for team docs"
```

### 6. View version history

```bash
demarkus -X VERSIONS mark://docs.internal.example.com/runbooks/deploy.md
```

Fetch a specific version:

```bash
demarkus mark://docs.internal.example.com/runbooks/deploy.md/v3
```

## Tips

- Scope tokens narrowly — per-person or per-directory
- Use `-label` when generating tokens for audit clarity
- `demarkus-tui` graph view (`d` key) shows document relationships
- Version history is permanent — there's no delete, only archive

## Related

- [Install on Linux](/install/linux/)
- [Public hub scenario](/scenarios/public-hub/)
- [Getting Started](/getting-started/)
