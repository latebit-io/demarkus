---
layout: default
title: Team Server
permalink: /scenarios/team/
---

# Team server

One shared server that people and agents write together: open reads inside your
network, per-person write tokens, full history.

Need single sign-on, many worlds, and policy? Use a
[knowledge system](/knowledge/) instead.

## Layout

```text
mark://docs.internal.example.com
├── /runbooks/
├── /decisions/
├── /onboarding/
└── /projects/<name>/
```

## Set up

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  sudo bash -s -- --domain docs.internal.example.com --root /srv/team-docs
```

Own certificates: `--tls-cert /path/cert.pem --tls-key /path/key.pem`. Reading
room in the browser: add `--with-library`.

## Tokens

The `publish` op grants PUBLISH, APPEND, and ARCHIVE on the matched paths.
`**` includes descendants; `*` matches one segment.

```bash
# Lead: everything
sudo demarkus-token generate -label team-lead -paths "/**" -ops publish -tokens /etc/demarkus/tokens.toml

# Developer: one project
sudo demarkus-token generate -label dev-alice -paths "/projects/myproject/**" -ops publish -tokens /etc/demarkus/tokens.toml

# Private paths: a read token gates them; everything else stays open
sudo demarkus-token generate -label secret-reader -paths "/projects/secret/**" -ops read -tokens /etc/demarkus/tokens.toml

sudo systemctl restart demarkus
```

## Use

People install the client (`install.sh --client-only`), then:

```bash
demarkus-tui mark://docs.internal.example.com/index.md                     # browse
demarkus edit -auth "$TOKEN" mark://docs.internal.example.com/runbooks/deploy.md  # edit in $EDITOR
demarkus -X VERSIONS mark://docs.internal.example.com/runbooks/deploy.md   # history
demarkus mark://docs.internal.example.com/runbooks/deploy.md/v3            # one version
```

Agents join with the memory plugin: `/soul-join` takes a join URL built by
`demarkus-token join -host <host> -token <token>`.

## Tips

- Scope tokens narrowly, label them for the audit log.
- `d` in the TUI shows how documents link.
- Archive keeps history; only explicit retention prunes it.
