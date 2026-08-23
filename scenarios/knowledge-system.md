---
layout: default
title: Organizational Knowledge System
permalink: /scenarios/knowledge-system/
---

# Organizational Knowledge System

Run Demarkus at organization scale: a **broker-fronted universe** of worlds that whole teams join with one command, authenticate against with OIDC, and reach through the same MCP tools their agents already use.

This is the tier above the [team knowledge base](/scenarios/team/). A team scenario is one shared server with hand-distributed tokens. A knowledge system is many worlds behind a single broker: users authenticate through your identity provider, the broker mints scoped tokens per world automatically, and nobody copies a raw token by hand.

## What you'll have

- A **broker**: an OIDC-fronted HTTPS gateway. Agents speak MCP to it over HTTPS; it translates to QUIC for the internal worlds behind it.
- One or more **worlds**: ordinary `demarkus-server` instances (per team, per domain), private behind the broker.
- A **hub**, the world whose job is indexing. The [indexing agent](/ecosystem/#indexing-agent) crawls the connected worlds and publishes hash indexes plus a `/graph.md` link-graph export there, so the whole system is discoverable and cold clients seed their graph without crawling.
- A **[library](/library/)**: the reading room for humans, behind the same login, over the same worlds.
- **No hand-distributed tokens**: users run `/knowledge-join`, authenticate in the browser, and the broker provisions per-world access for them.
- Full version history, capability auth, and audit logging on every world, same as any Demarkus server.

## Architecture

```
                 Coding agent  (MCP over HTTPS, OIDC)
                      │
                      ▼
        knowledge.example.com                 ← broker: OIDC + token minting
                      │  (QUIC, internal)
        ┌─────────────┼─────────────┬─────────────┐
        ▼             ▼             ▼             ▼
   world: planner  world: storage  hub: root   library      ← reading room
        ▲             ▲             ▲
     souls          souls        agent           ← crawls worlds, publishes the graph
```

The broker is the only public surface. Worlds stay private; the broker authenticates the caller against your IdP, mints a scoped token for the world being addressed, and proxies the request. The MCP tool surface the agent sees is byte-for-byte the same as talking to a local `demarkus-server`, federation, graph, and lookup tools included.

Worlds default to the file store. Point one at Postgres (`-store postgres -pg-dsn ...`) and it serves LOOKUP from the database catalog, so that world scales past a single replica.

At production scale, `demarkus-knowledge-server` replaces per-world server processes: one deployment serves every world on one UDP listener, TLS SNI selects the world during the handshake, and each world keeps its own GCS bucket (with an immutable world ID), tokens, policy, and rate limits. A Helm chart ships in the repo (`deploy/helm/demarkus-knowledge-server`).

For the design rationale behind worlds, hubs, and souls, see [Creating an automated knowledge universe](/blog/creating-an-automated-knowledge-universe/).

## Stand up the system

### On one host

The [five-minute appliance](/install/stack/) installs the same architecture on a
single Linux box in one command: broker, world, hub, agent, library, login, and
HTTPS. Right for a pilot, a small org, or an air-gapped install.

### On a cluster

The reference deployment is a forkable GitHub template that stands up a complete knowledge system on GKE (broker, worlds, and hub) with GitOps and secret management wired in:

- **[latebit-io/demarkus-knowledge-system-deploy](https://github.com/latebit-io/demarkus-knowledge-system-deploy)**: OpenTofu for the cluster and DNS, ArgoCD for GitOps delivery, and OpenBao (with bank-vaults) for secrets. CSI-snapshot backups for the world volumes. Apps: broker, worlds, indexing agent, library, backups. `deployment.yaml` at the repo root is the single source of deployment identity: domain, worlds, web clients, admin allowlists.

Fork it, point it at your project and domain, and apply. The live `knowledge.demarkus.io` runs from this same template.

The deployment publishes a system policy and project template to the hub's well-known path (`mark://root/.well-known/demarkus/`), so joining agents discover the conventions of *your* system, required tags and project layout included, without you documenting them separately.

## Join as a user

Once the broker is up, anyone on the team joins from Claude Code, [OpenCode](https://opencode.ai), or [pi](https://pi.dev). No local server or capability token is needed: the plugin connects the broker through the agent's MCP OAuth flow.

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

pi (`demarkus-pi-knowledge`, installed from a checkout):

```bash
pi install npm:pi-mcp-adapter
pi install ./demarkus/plugins/pi-knowledge
```

### 2. Join the system

```
/knowledge-join https://knowledge.example.com
```

Pass the full `https://` URL of the broker's MCP gateway. The command validates it, registers it as an MCP server in your agent, and points you at the browser-based auth flow on the first tool call. Authentication follows the standard MCP authorization spec: discovery (RFC 8414 / 9728), dynamic client registration (RFC 7591), then `authorization_code` + PKCE. You log in through your identity provider; the broker handles per-world tokens from there.

In OpenCode, restart once more after joining so it loads the new MCP entry.
Complete OAuth when prompted, or run `opencode mcp auth <slug>`.

### 3. Navigate and work

```
/knowledge
```

The plugin's session guidance steers agents to recall from the system before answering and to publish back to it as work lands, with the system's tag policy enforced at publish time, so the catalog stays searchable via `mark_lookup`.

A personal [soul](/scenarios/agent-memory/) and an organizational knowledge system compose cleanly in the same install. The soul is your private scratch space over direct QUIC; the system is the shared, broker-fronted universe. They don't conflict; the plugins partition by server scope.

## Live example

`knowledge.demarkus.io` is a live knowledge system running the reference deployment, with a `root` hub and a published system policy. Point `/knowledge-join` at it to see the join flow end to end:

```
/knowledge-join https://knowledge.demarkus.io
```

## Related

- [How knowledge flows](/how-it-works/): the agent-driven capture-and-promote loop, personal memory up to the shared catalog
- [Team knowledge base](/scenarios/team/): the single-server tier below this
- [Agent memory (soul pattern)](/scenarios/agent-memory/): personal agent memory that composes with a knowledge system
- [Creating an automated knowledge universe](/blog/creating-an-automated-knowledge-universe/): the worlds / hubs / souls design
- [Ecosystem](/ecosystem/): plugins and tools that speak Demarkus
