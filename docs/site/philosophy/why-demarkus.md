# Why Demarkus

Demarkus exists because the modern web is optimized for **attention and monetization**, not for durable knowledge. If you want information to be portable, verifiable, and **co‑authored by humans and AI agents**, Demarkus offers a cleaner path.

## The Problem It Solves

Most web content today is:

- Wrapped in heavy rendering layers
- Mutable without audit trails
- Loaded with tracking
- Difficult for agents to parse reliably

Demarkus replaces that with a simple protocol for **plain, structured text** delivered over secure transport.

## Why Choose It

### 1) Durable Knowledge
Every write produces a new immutable version. History is append‑only, so you can always verify what changed and when.

### 1a) AI + Human Collaboration (Examples)
Demarkus makes it easy to build agent workflows that humans can verify and curate:

- **Agent auto‑summaries**: publish summaries alongside source docs for fast review.
- **Link graph validation**: crawl and flag broken links across a knowledge base.
- **Document enrichment**: generate tags, glossaries, or cross‑refs as new versions.
- **MCP workflows**: connect LLM tools to `mark_fetch`/`mark_publish` for controlled edits.
- **Review loops**: agents propose updates, humans approve and publish new versions.

### 2) Agent‑Friendly by Design
Markdown is easy to parse and predictable to traverse. Documents link together naturally to form a graph that humans can curate and agents can reason over and expand.

### 3) Minimal Infrastructure
The server is small and runs anywhere. No databases, no CMS, no background services required.

### 4) Privacy‑Respecting Delivery
No tracking, no analytics, no embedded scripts. Just content over encrypted transport.

### 5) Verifiable Integrity
Version chains are linked by hashes. If any historical version is modified, the chain breaks and is detectable.

### 6) Open and Federated
Anyone can run a server. Documents can be mirrored. There is no central authority.

## When It’s a Good Fit

Demarkus is a strong fit for:

- Personal knowledge bases
- Research archives and documentation
- Agent‑native content delivery
- Internal docs with auditability requirements
- Public publishing without platform lock‑in

## When It’s Not

Demarkus is **not** designed for:

- Rich client‑side applications
- Social feeds or dynamic user interaction
- Real‑time collaboration (yet)

## The Core Tradeoff

Demarkus chooses **clarity and integrity** over flexibility and client‑side interactivity. That tradeoff is the point.

## Related

- [Philosophy & Intent](index.md)
- [Architecture & Design](../architecture/index.md)
- [Protocol Specification](../../spec.md)
- [Docs Home](../index.md)