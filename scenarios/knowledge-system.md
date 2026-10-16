---
layout: default
title: Organizational Knowledge System
permalink: /scenarios/knowledge-system/
---

# Organizational Knowledge System

Run Demarkus at organization scale: a **broker-fronted universe** of worlds that whole teams join with one command, authenticate against with OIDC, and reach through the same MCP tools their agents already use.

This is the tier above the [team knowledge base](/scenarios/team/). Users sign in through your identity provider; the gateway checks each world's access and write scope. Nobody copies a raw world token by hand.

## What you'll have

- **`demarkus-knowledge`**: one process hosting worlds, OIDC login, and MCP gateways.
- One or more **worlds**: isolated catalogs, each with its own GCS bucket, policy, and access rules.
- A **hub**: the world holding shared conventions and the cross-world graph, derived by the knowledge server.
- A **[library](/library/)**: the reading room for humans, behind the same login, over the same worlds.
- **No hand-distributed tokens**: users run `/knowledge-join` and authenticate in the browser.
- Full version history, capability auth, and audit logging on every world, same as any Demarkus server.

## Architecture

```
     Coding agents          Library
            │                  │
            └──── MCP + OIDC ───┘
                       │
              demarkus-knowledge
          gateways + worlds + graph
                       │
             ┌─────────┼─────────┐
             ▼         ▼         ▼
           root      planner   storage
              one GCS bucket per world
```

Gateway calls reach worlds in process. `mark_worlds` lists readable worlds;
`mark_lookup_all` searches them in one call and reports partial failures. Lookup
matches tags and titles, or section text with `match=body`; `budget` can include
focused passages and related documents.

Direct QUIC clients select a world with TLS SNI. Capability tokens are for direct
clients; gateway writes use the signed-in identity's world grant.

### High availability

Durable state lives in GCS. World writes commit through a compare-and-swap head
and commit log; replicas catch up from the bucket. No persistent volumes or
writer leader election. The chart requires at least two replicas and keeps one
available during voluntary disruption.

The server initializes an empty world on first start and seeds its write policy.
The floor world's policy applies across the system; each world can tighten it.
Writes are checked for secrets and personal data. GCS access uses workload
identity; direct-client tokens, when needed, mount from Secrets.

For the design rationale behind worlds, hubs, and memories, see [Creating an automated knowledge universe](/blog/creating-an-automated-knowledge-universe/).

## Stand up the system

Use the [knowledge-system Helm chart](/install/stack/): one values file declares
the worlds, OIDC gateway, and optional library. The
[deployment repository](https://github.com/latebit-io/demarkus-knowledge-system-deploy)
provides the GKE and GitOps setup.

The deployment publishes a system policy and project template to the hub's well-known path (`mark://root/.well-known/demarkus/`), so joining agents discover the conventions of *your* system, required tags and project layout included, without you documenting them separately.

## Join as a user

Once the broker is up, anyone on the team joins from Claude Code, [Cursor](https://cursor.com), [OpenCode](https://opencode.ai), or [pi](https://pi.dev). No local server or capability token is needed: the plugin connects the broker through the agent's MCP OAuth flow.

### 1. Install the plugin

Claude Code:

```
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-knowledge@demarkus
```

OpenCode:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash
```

Restart OpenCode before continuing.

pi:

```bash
pi install npm:pi-mcp-adapter
pi install git:github.com/latebit-io/demarkus-pi-knowledge
```

Cursor, with the repository added as a marketplace from the plugins panel (or symlinked from a checkout into `~/.cursor/plugins/local/`):

```
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-knowledge → Add
```

Restart Cursor before continuing.

### 2. Join the system

```
/knowledge-join https://knowledge.example.com
```

Pass the full `https://` URL of the MCP gateway. The command registers it in your
agent and starts the browser-based OAuth flow. You log in through your identity
provider; the gateway enforces access per world.

In OpenCode, restart after joining. In V2, open `/mcps`, select the server, and
sign in; in V1, complete OAuth when prompted or run `opencode mcp auth <slug>`.
In Cursor, restart and complete OAuth under Settings → MCP.

### 3. Navigate and work

```
/knowledge
```

The plugin steers agents to consult the system first on shared subjects. Ready
material lands through `/promote` and a human gate, with the system's policy
enforced at write time.

A personal [memory](/scenarios/agent-memory/) and a knowledge system compose in
the same agent. Draft in your memory; promote what others should rely on.

## Live example

`knowledge.demarkus.io` is a live knowledge system running the reference deployment, with a `root` hub and a published system policy. Point `/knowledge-join` at it to see the join flow end to end:

```
/knowledge-join https://knowledge.demarkus.io
```

## Related

- [How knowledge flows](/how-it-works/): the agent-driven capture-and-promote loop, personal memory up to the shared catalog
- [Team knowledge base](/scenarios/team/): the single-server tier below this
- [Agent memory](/scenarios/agent-memory/): personal agent memory that composes with a knowledge system
- [Creating an automated knowledge universe](/blog/creating-an-automated-knowledge-universe/): the worlds / hubs / memories design
- [Ecosystem](/ecosystem/): plugins and tools that speak Demarkus
