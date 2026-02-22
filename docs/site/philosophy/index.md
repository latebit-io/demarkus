# Philosophy & Intent

Demarkus is built around a simple premise: **information should move as plain, verifiable text over a secure transport**, without tracking, manipulation, or platform lock‑in. This guide explains the values behind the project and why you might choose it.

## Intent

Demarkus rethinks the web as a **document protocol**, not a rendering system. The goal is a clean, durable, agent‑friendly medium that works for humans and machines alike:

- **Humans** get readable, portable markdown.
- **Agents** get structured, predictable text.
- **Together** they collaborate through the same document graph — humans write and curate, agents read, index, and assist.
- **Operators** get a minimal, secure, auditable server.

## Core Principles

### 1) Optimized for Information
Markdown is the shared language. It is structured enough for agents and simple enough for humans. The protocol is designed to carry content directly, without intermediate rendering pipelines.

### 2) Privacy First
No tracking, no user profiling, no analytics baked in. Demarkus doesn’t assume identities or sessions — just documents over encrypted transport.

### 3) Security is Foundational
Transport is always encrypted. Paths are validated. Authentication is capability‑based: tokens grant **actions on paths**, not identities.

### 4) Simplicity Over Complexity
The protocol is human‑readable. The server is small. The client is direct. The goal is **low cognitive load**, not feature sprawl.

### 5) Integrity Over Mutability
Every write creates a new immutable version. Version history is append‑only. This prevents silent revisionism and supports independent verification.

### 6) Anti‑Commercialization
No ad‑driven incentives. No central authority. The protocol is designed to be neutral infrastructure, not a platform.

### 7) Federation by Default
Anyone can run a server. Documents can be mirrored freely. Resilience emerges from distributed access, not centralized control.

## Why Use Demarkus

Choose Demarkus if you want AI + human collaboration on a shared, verifiable knowledge base:

- **Durable documents** with verifiable history
- **Agent‑friendly knowledge** that is easy to parse and index
- **Minimal infrastructure** that runs anywhere
- **Privacy‑respecting delivery** without tracking
- **A clean protocol surface** you can build on

### AI + Human Collaboration Examples

- **Agent auto‑summaries**: generate concise summaries alongside human‑written documents.
- **Link graph validation**: agents crawl and flag broken links or missing pages.
- **Knowledge base synthesis**: agents propose new pages from existing material; humans review and publish.
- **MCP workflows**: use `demarkus-mcp` to let agents fetch, list, and publish content through tools.

### Agent-to-Agent Collaboration (Read/Write Loops)

- **Shared queues**: one agent publishes tasks to `/docs/queue.md`, others fetch and process.
- **Handoff chains**: Agent A drafts, Agent B refines, Agent C verifies — each publishes a new version.
- **Structured outputs**: agents write summaries, tags, or validation reports as separate docs linked to sources.
- **Coordination by links**: agents discover and coordinate work by traversing the document graph.

## What Demarkus Is Not

- It is **not** a web browser or a rendering framework.
- It is **not** a social platform.
- It is **not** designed for dynamic client‑side execution.

## Design Consequences

These values drive concrete choices:

- **Text status codes** instead of numeric HTTP codes
- **Immutable versions** instead of in‑place edits
- **Capability tokens** instead of accounts
- **QUIC transport** instead of HTTP

## Related

- [Why Demarkus](why-demarkus.md)
- [Agent Workflow Cookbook](agent-cookbook.md)
- [Agent Queue Template](agent-queue-template.md)
- [Architecture & Design](../architecture/index.md)
- [Protocol Specification](../../spec.md)
- [Docs Home](../index.md)