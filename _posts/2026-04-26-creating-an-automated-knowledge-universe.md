---
layout: default
title: "Creating an automated knowledge universe"
date: 2026-04-26
permalink: /blog/creating-an-automated-knowledge-universe/
---

# Creating an automated knowledge universe

Humans are bad at documentation. Really bad. Not some of us, most of us. Maybe 1% of engineers consistently write and maintain accurate docs. The rest write it once and have intention of maintaining, but never do.

We develop or use systems that assume otherwise. Wikis, Confluence spaces, Notion databases, README files, all with intention to create up-to-date knowledge bases.

There is a another problem that has grows quietly alongside the first. Knowledge is scattered across too many tools, each with its own structure, its own search, its own way of keeping information locked up. When AI agents arrived, we bolted some MCP tools on expecting them to navigate these heavy weight systems that were never designed for machines.

To compound knowledge issues.\, Agents have short lived memories. Every session starts cold. Context that lives in one team is invisible to another. An agent working on a feature has no idea what a neighbouring team decided last week, even when that decision spans over many teams or users and can have implications. 

We keep asking how to get context across teams? How do we give agents better memory? We keep reaching for tools developed for legacy ways of humans interacting with knowledge to solve it.

Time to stop doing that.

---

## Let Agents Do What They Are Good At

Agents are good at producing and maintaining structured information. They do not get bored. They do not skip writing the decision record because the sprint is ending. They do not forget to update the doc when the code changes (most of the time?). Given the right tools and the right protocol, agents will build and maintain a knowledge base that humans can't.

The missing piece has been the protocol. Agents have been trying to work with tools designed for people, web interfaces, search engines, version control systems, and the friction is real. What agents need is something they can genuinely master: a simple, predictable, composable protocol that maps to how they already think.

That is what demarkus is.

---

## What is demarkus

demarkus (de-centralized markdown for us) is a protocol built for agents and humans reading and writing markdown documents. Not a platform, not a knowledge base you buy and configure. A protocol, like HTTP, but for knowledge.

Think of a demarkus server as a bookshelf. Not a database, not version control. A bookshelf. You put documents on it, you take documents off it, you easily see what is in the library. That is the whole mental model. The simplicity is the point.

When a server is simple you can solve complexities on top of it. When a server starts complex it can only do that one complex thing and stays rigid forever (confluence?). Agents also understand simple very well. A complete, predictable interface fits in a single context window and agents become genuine experts at using it fast.

### Versioning is Non-Negotiable

demarkus versions every document. Agents hallucinate. Agents make mistakes. People make mistakes. When an agent publishes something wrong you want to know exactly what changed, when, and be able to go back. Every write creates a new immutable version, v1, v2, v3, with a content hash chain. No branching, no merge conflicts, no reconciliation. Just an auditable history of everything any agent wrote.

### The Wire Format

Every interaction has three parts. A verb and a path telling the server what you want to do. A YAML frontmatter block with metadata like who published and version numbers. A markdown body with the content. Responses come back the same way, frontmatter with the status, body with the content. Agents understand this immediately. It maps to how they already think.

### Six Verbs

```
FETCH      Read a document
LIST       Discover what exists at a path
VERSIONS   Get the full version history
PUBLISH    Create or update a document
APPEND     Add to an existing document without resending it
ARCHIVE    Mark a document as superseded
```

The verb set is complete and closed. Every missing verb is a class of bugs that cannot exist. All six map directly to MCP tools. There is one additional MCP tool not tied to a verb: mark_graph, which crawls a server and builds a document graph for discovery. Six verbs, seven tools, and agents can master all of them in one session.

### Lightweight by Design

A demarkus server is a single binary. No database to configure, no cloud dependency, no license fee. Because spinning one up costs almost nothing, you can run many of them, one per team, one per domain, one per developer, and each one stays focused on its own concern.

---

## Building Worlds

Once you have simple servers everywhere, you build worlds.

A world can be team's or group's server. Their knowledge base, their decisions, their documents. The compiler team has a world. The API team has a world. The infra team has a world. Each one is independent by default, owned by that team, written to by their agents.

Worlds are not locked. Documents in one world can link directly to documents in another using mark:// URIs. Worlds can copy documents when they need local context from a neighbouring domain. Because every server speaks the same protocol, agents move between them without friction.

### Hubs Connect Worlds

You connect worlds through hubs. A hub is just another demarkus server whose job is indexing. Hub agents crawl the connected worlds, build document graphs, and publish cross-references that make the whole network discoverable.

When a compiler team agent publishes a decision about a new IR format, the hub discovers it. When a codegen agent needs to understand what constraints apply to its output, it asks the hub. No ticket. No meeting. No waiting.

The hub is a formal way of connecting worlds, but it is not the only way. Worlds can link directly. The hub adds an indexing layer, a place where cross-domain relationships are named and navigable, but the connections exist in the documents themselves.

### Developer Souls

Around each world orbit the developers or knowledge creators, they have their own personal servers, their local agent memory. These are scratch pads.

An engineer working on a feature drafts notes on their local soul first. The agent refines across sessions, building context, connecting to decisions from other domains. When the work is ready the agent publishes to the team world. The local "soul" remembers everything across sessions. The team world holds what has been agreed. The hub makes it all discoverable.

The soul pattern solves the agent memory problem without special infrastructure. A local demarkus server is persistent memory for an AI agent that costs nothing to run and speaks the same protocol as every other node in the network.

---

## The Universe Pattern

At scale, demarkus networks become something you can reason about clearly.

Teams have worlds. Worlds connect to hubs. Developers have souls that orbit their team's world. The whole structure is a distributed knowledge graph, every node speaking the same protocol, every document addressable from anywhere by content hash.

Agent interactions happen in parallel. An agent in one team appends a session note to its soul while a hub agent crawls all the connected worlds and publishes updated cross-references. None of this needs coordination. By the time you need context, it is already there.

---

## In Practice: synapse

Consider a fictional distributed systems company called synapse, building a query engine. Three teams: the query planner, the storage engine, and the wire protocol.

The query planner team publishes an ADR about how predicate pushdown will work, what the optimizer can assume, what it cannot. Their world holds the design docs, the decision log, the rationale.

The storage engine team is working on a new block format. Their agent fetches the hub graph and finds the cross-reference to the predicate pushdown ADR. It reads the document, understands the constraints the planner relies on, and factors them into the block format design. The planner team never filed a ticket. The storage team never asked. The context flowed through the protocol.

The wire protocol team has a similar story. Their decisions about encoding affect both teams above them. When they publish a change, the hub indexes it. Both downstream teams discover it on their next hub query and update their own documents to reflect the new constraints.

Three developer souls orbit the three worlds. Engineers draft locally, refine across sessions, publish when ready. The hub holds the cross-domain picture. New engineers joining the team do not need to ask what was decided or why. Their agent fetches the hub graph and reads it.

---

## Getting Started

Everything you need to get up and running is at [demarkus.io](https://demarkus.io) including installation, server setup, CLI usage, and MCP integration.

---

## The Point

Stop building knowledge systems that will never be maintained. Build for agents who will do it continuously.

Give agents a protocol they can master. Give them servers simple enough to reason about completely. Give them the tools to read, write, version, and connect knowledge across any boundary.

The knowledge base will not be the one you designed. It will be the one your agents built with your guidance. And it will stay current because agents, unlike most humans, do not forget to update the documentation (most of the time?).
