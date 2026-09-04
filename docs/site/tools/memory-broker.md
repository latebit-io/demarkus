# Memory Broker (`demarkus-memory-broker`)

Memory as a service for MCP hosts: one OAuth connector URL gives any MCP host a private, versioned personal memory. The memory broker shares its libraries and deployment profile with the knowledge broker but runs a different authorization model: your identity maps to exactly one world, and reads AND writes are locked to it. Nothing is org-open, and cross-tenant access is denied at the tool, resource, and crawl layers.

On your first authenticated tool call the broker seeds your memory with a template (`/index.md` hub, a low-ceremony write policy, and the memory layout at `/.well-known/demarkus/template.md`). With dynamic provisioning enabled, the first arrival also creates the world itself: bucket, server registration, and tokens, behind a provisioning gate (`static | allowlisted | open`).

See `tools/demarkus-memory-broker/MCP-API.md` for the tool contract and the Helm chart README (`deploy/helm/demarkus-memory-broker/`) for deployment.

## Connecting a host

The endpoint is the broker's MCP gateway URL, for example `https://memory.example.com/mcp`. Authentication is standard MCP OAuth: the host discovers the flow from the endpoint's `WWW-Authenticate` challenge and RFC 9728 metadata; no manual token handling.

### Claude Desktop

Settings > Connectors > Add custom connector, with the gateway URL. Desktop walks the OAuth flow in the browser; after that the memory tools (`mark_fetch`, `mark_publish`, ...) and prompts (`soul-context`, `soul-journal`, `soul-export`) appear. Usage guidance rides in the server instructions, so no client-side configuration is needed.

### ChatGPT

Requires a Business, Enterprise, or Edu workspace with Developer mode authorized by the workspace admin; custom connectors are added through the Apps creation flow with the same gateway URL and OAuth flow. Consumer ChatGPT plans cannot add custom MCP connectors with write support.

### Claude Code

With the demarkus-memory plugin, `/soul-join https://memory.example.com` validates the endpoint, registers the catalog row, binds the project (the destination gate then routes memory writes here), and registers the HTTP MCP server. Without the plugin:

```bash
claude mcp add --transport http soul https://memory.example.com/mcp
```

The OAuth flow runs in the client on first use; no token files.

### Cursor / Windsurf

Add a remote MCP server with the gateway URL; both support the same streamable HTTP + OAuth shape.

## First-call flow

1. Authenticate (OAuth in the host).
2. Call `mark_worlds` once: it returns your world name, the `{worldName}` every other tool addresses as `mark://{worldName}/{path}`.
3. Recall with `mark_lookup` / `mark_fetch`; record with `mark_publish` / `mark_append`.

With dynamic provisioning, a brand-new identity's first call may answer "your memory world is being provisioned; try again in about a minute" while the backend picks the new world up.

## Getting your data out

The `soul-export` prompt walks every document in your world and saves each one verbatim to local files with a manifest. The memory is yours; the service is not a lock-in.
