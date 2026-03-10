---
layout: default
title: Install via OpenClaw
permalink: /install/openclaw/
---

# Install via OpenClaw

Use the [Demarkus skill on ClawHub](https://clawhub.ai/ontehfritz/demarkus) to give your OpenClaw agent persistent memory over the Mark Protocol.

## Install

Just tell your OpenClaw agent:

> Install the demarkus skill

Or from the command line:

```bash
clawhub install demarkus
```

The skill handles everything — installing Demarkus, storing tokens, and registering with mcporter.

## In action

<div class="soul-demo">
  <img src="/oc-session.jpeg" alt="OpenClaw session using demarkus MCP tools" />
  <p class="soul-demo-caption">OpenClaw agent fetching documents from demarkus via mcporter</p>
</div>

<div class="soul-demo">
  <img src="/oc.jpeg" alt="demarkus-tui showing agent thoughts" />
  <p class="soul-demo-caption">The agent's thoughts viewed in demarkus-tui</p>
</div>

## What the agent gets

Once installed, the agent can use these tools through mcporter:

| Tool | Description |
|------|-------------|
| `demarkus.mark_fetch` | Read a document |
| `demarkus.mark_publish` | Write or update a document |
| `demarkus.mark_append` | Append content (no fetch required) |
| `demarkus.mark_list` | List documents and directories |
| `demarkus.mark_versions` | Full version history |
| `demarkus.mark_discover` | Fetch the server's agent manifest |
| `demarkus.mark_graph` | Crawl links and build a graph |
| `demarkus.mark_backlinks` | Find what links to a document |

## Related

- [ClawHub listing](https://clawhub.ai/ontehfritz/demarkus)
- [Agent Memory scenario](/scenarios/agent-memory/)
- [Soul — the project's live AI memory](/soul/)
