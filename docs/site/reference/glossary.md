# Glossary

Audience: anyone reading Demarkus docs, CLI help, or tool descriptions. Every concept has exactly one name; this page defines each and lists the older names it replaces.

Demarkus is one engine with two products on top and a bridge between them. The engine is the Mark Protocol and its servers. The products are memory (personal) and knowledge (shared). The bridge is promote and refresh.

## Engine

- **Mark Protocol**: the wire protocol. Versioned markdown documents over QUIC, addressed as `mark://<world>/<path>`. Specified in [SPEC.md](../../SPEC.md).
- **world**: one server's versioned document store, the unit of storage and of addressing. A world has a name, a set of documents, a hash chain per document, and its own tokens and policy. `demarkus-server` hosts one world; `demarkus-knowledge-server` hosts many in one process.
- **document**: one markdown file in a world, with a full version history. Metadata (title, tags, importance, type, typed relations) travels out of band, not in the body.
- **hub**: a document, conventionally `index.md`, whose links are the entry point to a world or to a set of worlds. Hubs can link to hubs.
- **server**: a process that serves one or more worlds over the Mark Protocol. Servers are storage engines: they hold documents and hand them back; they do not search, rank, or route.
- **agent**: `demarkus-agent`, the indexer. It crawls worlds, aggregates the link graph, and publishes hash indexes and hub documents. Not to be confused with an AI agent using the MCP tools.
- **capability token**: the write credential. Tokens are scoped to a world and a path prefix; reads need no token unless the world enables read auth.

## Memory

- **memory**: a world owned by one identity, a person or an AI agent. Private by default. It records decisions, lessons, and progress across sessions and recalls them later. Replaces the older term *soul*.
- **local memory**: a memory served by a `demarkus-server` that the memory plugin runs on your machine. Zero configuration.
- **hosted memory**: a memory provisioned by the memory broker, one world per signed-in identity, reachable over MCP with OAuth from any MCP host.
- **memory plugin**: the per-host adapter (Claude Code, OpenCode, pi) that installs the memory tools, standing guidance, gates, and its slash commands.
- **memory broker**: `demarkus-memory-broker`, the OIDC-fronted MCP gateway that provisions and serves hosted memories.

## Knowledge

- **knowledge system**: an organization's shared, versioned knowledge base: many worlds composed behind one HTTPS endpoint with single sign-on. Humans and agents read and write the same catalog. Replaces the older term *universe*.
- **knowledge server**: `demarkus-knowledge-server`, the production server that hosts many isolated worlds in one process, selected by TLS SNI, each backed by its own object-storage bucket.
- **knowledge broker**: `demarkus-knowledge-broker`, the OIDC-fronted MCP gateway that composes worlds into a knowledge system and answers system-wide catalog lookups.
- **knowledge plugin**: the per-host adapter that joins a knowledge system with one command and adds knowledge-first guidance and its slash commands.
- **root**: the conventional system namespace of a brokered knowledge system. `mark://root/.well-known/demarkus/` holds the write policy, the per-world template, and the style guide.
- **policy**: a world's write rules: strictness and required tag axes, enforced at publish time.
- **library**: the web reading room, a server-rendered front end for browsing a knowledge system or a single world through the broker with single sign-on.

## Bridge

- **promote**: lift a memory document into a knowledge system: curate, deduplicate, tag to the shared taxonomy, route to a writable world, gate with a human, publish with provenance, and back-stamp the source.
- **refresh**: pull the authoritative knowledge copy of a promoted document back into the memory it came from. Knowledge is authoritative; memory refreshes from it.

## Operations

- **single-host install**: server, broker, library, identity provider, and HTTPS on one machine from one command. Replaces the older term *appliance*.
- **catalog**: the per-world index that `LOOKUP` searches: titles, tags, and importance. Not full-text search. A document that was never tagged or titled is not in the catalog.
- **graph**: the persistent link graph across worlds, built by the agent and queried through backlinks, related documents, and exports.
- **tenant**: broker-internal record for one hosted memory identity. Operator documentation only; never user-facing.

## Retired terms

| Older term | Use instead |
|---|---|
| soul | memory |
| universe | knowledge system |
| appliance | single-host install |
| librarian | the library's agent, or simply the agent |

Command names and MCP prompt names that carried the older terms remain as aliases for one release after the rename.

Glossary exceptions, kept on purpose: the single-host installer and its page are still called the appliance until the installer is renamed; on-disk plugin state under `~/.demarkus` (`souls`, `project-souls`, `SOUL_DIR`) and the memory subdomain of installs made before the rename (`soul.<host>`) keep their names so existing installs work unchanged; `soul.demarkus.io` is the project's hostname.
