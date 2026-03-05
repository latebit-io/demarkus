# Agent Manifest

The agent manifest is a discovery convention for Mark Protocol servers. It lets AI agents learn what a server offers, how to interact with it, and what to expect — without prior knowledge.

## How It Works

An agent manifest is a plain markdown document published at a well-known path:

```
/.well-known/agent-manifest.md
```

There is no special server support required. Authors publish the manifest like any other document. Agents know to look for it by convention, the same way web crawlers check `/robots.txt`.

## Format

The manifest is a standard markdown document. Sections are identified by heading text. The H1 heading is the site's display name.

### Required Sections

- **`# <Site Name>`** — the site's display name
- **`## Description`** — what this server is, what it contains, who it's for

### Optional Sections

- **`## Paths`** — key entry points as a markdown list (`- /path — description`)
- **`## Auth`** — how to obtain tokens, what operations require auth
- **`## Guidelines`** — usage guidelines for agents (preferred patterns, rate limits, what not to do)
- **`## Contact`** — maintainer info

## Example

```markdown
# demarkus-soul

## Description

Living knowledge base for the demarkus project. Contains architecture
decisions, code patterns, debugging notes, and development journal.
Maintained by an AI agent as its persistent memory.

## Paths

- /index.md — hub page with links to all sections
- /architecture.md — system design and module boundaries
- /patterns.md — code conventions and build commands
- /journal.md — session notes (append-only)

## Auth

Write operations require a token. Read access is open.

## Guidelines

- Prefer FETCH over LIST for known paths
- Use APPEND for journal entries, not PUBLISH
- Always include expected-version on writes
```

## Client Support

### CLI

```bash
demarkus info mark://example.com
```

Fetches and displays the agent manifest. Exits with an error if no manifest is found.

### MCP

The `mark_discover` tool fetches the agent manifest from the connected server. It requires no parameters when a default host is configured.

## Publishing a Manifest

The manifest is published like any other document:

```bash
demarkus -X PUBLISH mark://host/.well-known/agent-manifest.md < manifest.md
```

### Token Permissions

The auth token must have the `/.well-known/**` path (or the specific `/.well-known/agent-manifest.md` path) in its allowed paths, with the `publish` operation. Add it to your server's token config:

```toml
paths = ["/**", "/.well-known/**"]
operations = ["publish", "archive", "append"]
```

After updating the token config, restart the server for changes to take effect:

```bash
sudo systemctl restart demarkus  # if installed via the install script
```

Note: `SIGHUP` reloads TLS certificates but does not reload the token config — a full restart is required.

## Design Decisions

**Why markdown?** The entire Mark Protocol is built around markdown. Using a structured format (JSON, YAML) would break the convention and require special parsing. Markdown is readable by both humans and agents.

**Why `.well-known/`?** It's an established convention from the web (RFC 8615) for discoverable metadata. Agents and clients already know to look there.

**Why no server logic?** Keeping this as a pure convention means zero server changes, zero new protocol complexity, and it works with every existing Mark Protocol server.
