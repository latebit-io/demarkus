# Knowledge Base

One shared, versioned catalog for your organization. People and agents read and write the same documents. Teams sign in with your identity provider and join with one command.

- **Shared**: many worlds, one per team or domain, searched in one call.
- **Curated**: notes arrive from personal memory through a review gate.
- **Governed**: your policy holds on every write: tags, layout, no secrets, no duplicates.
- **Linked**: one [knowledge graph](../graph/index.md) across every world.
- **Readable**: humans browse it in the [library](https://github.com/latebit-io/demarkus-library).

## How knowledge flows

```text
  agent ──record──▶ MEMORY ──/promote──▶ KNOWLEDGE BASE ──▶ LIBRARY
    ▲               private   review     shared            people
    └───────────────── consult first ◀──────────┘
```

The agent drafts in [memory](../memory/index.md) as it works. What others should rely on moves up through `/promote`. On a shared subject the agent consults the knowledge base first.

## Join

Install the knowledge plugin next to the memory plugin.

| Agent | Install |
|---|---|
| Claude Code, Cowork | `/plugin install demarkus-knowledge@demarkus` |
| Cursor | Plugins panel → `demarkus-knowledge` → Add |
| OpenCode | Installer below, then restart |
| pi | `pi install git:github.com/latebit-io/demarkus-pi-knowledge` |

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge/install.sh | bash
```

Then join your system and sign in through the browser:

```text
/knowledge-join https://knowledge.example.com
```

No server to run, no token to copy. Access is checked per world. In Cowork, join from a terminal, or have an admin add the system's `/mcp` URL as a connector.

Try it against the live system: `/knowledge-join https://knowledge.demarkus.io`

## Work

- **Consult.** `mark_lookup_all` searches every world you can read in one call. Partial failures stay visible.
- **Navigate.** `/knowledge` lists joined systems and their hubs.
- **Promote.** `/promote <path>` distills a memory note, strips personal framing and secrets, checks for duplicates, tags it to your taxonomy, picks a writable world, and shows a dry run. You approve before it lands; each world sets how much an agent may do alone.
- **Refresh.** `/soul-refresh` pulls the shared copy back down. The knowledge base stays authoritative.
- **Audit.** `/knowledge-doctor` finds orphans, broken links, untagged or off-policy documents, secrets, and duplicates.

## Policy

The root world publishes the rules at `mark://root/.well-known/demarkus/`: `policy.md` (required tags and their allowed values), `template.md` (layout), and `style.md`. Joining agents read them; the server enforces them inside every commit. Each world can tighten the floor, never loosen it.

| Check | On a bad write |
|---|---|
| Required tags and fields | Warn, ask, or block, as the policy sets |
| Secrets | Refused, naming the rule and line. No override |
| Personal data | Refused by default; a reviewed false positive lands with a reason |
| Near-duplicate | Refused, naming the document that holds it; update that one, or override with a reason |
| Style | Notes on the result |

## Run one

`demarkus-knowledge` is one process: worlds, single sign-on, the MCP gateways, and the cross-world graph.

```text
   coding agents        library
         │                 │
         └── MCP + OIDC ───┘
                  │
         demarkus-knowledge
     gateways · worlds · graph
                  │
        ┌─────────┼─────────┐
        ▼         ▼         ▼
      root     platform   payments
       one GCS bucket per world
```

- **Stateless replicas.** State lives in GCS; writes commit by compare-and-swap. No volumes, no leader.
- **Worlds on demand.** An empty bucket becomes a world on first start, seeded with your policy.
- **One values file.** The Helm chart declares every world once. See [Kubernetes & Helm](../deployment/kubernetes.md).

The same binary serves **hosted memory**: each signed-in person gets a private memory world over MCP. See [Memory gateway](../tools/memory-broker.md).

## Related

- [Memory](../memory/index.md)
- [Knowledge graph](../graph/index.md)
- [Security Model](../security/index.md)
- [Docs Home](../index.md)
