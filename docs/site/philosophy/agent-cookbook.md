# Agent Workflow Cookbook

This cookbook provides practical, repeatable workflows for human + AI collaboration using Demarkus. Each recipe is designed to keep humans in control while letting agents do high‑leverage work like summarization, link validation, and structured publishing.

> All workflows assume a running Demarkus server and the `demarkus` CLI. If you’re using the MCP server, you can translate these steps into tool calls.

---

## Recipe 1: Agent Auto‑Summaries (Human Review)

**Goal:** Publish concise summaries alongside source docs.

**Pattern:**  
`/docs/article.md` → `/docs/article.summary.md`

**Workflow:**
1. Agent fetches the source document.
2. Agent generates a summary (short bullets or a short paragraph).
3. Human reviews summary for correctness and tone.
4. Human (or agent with approval) publishes summary as a new doc.

**Why it works:**  
Summaries provide fast context without changing the original document.

---

## Recipe 2: Link Graph Validation

**Goal:** Detect broken links across a documentation tree.

**Workflow:**
1. Agent crawls the graph starting at `/docs/index.md`.
2. Agent reports any `not-found` nodes.
3. Human decides whether to fix links or publish missing pages.
4. Updates are published as new versions.

**Why it works:**  
The document graph is the navigation system. Keeping it clean preserves usability and trust.

---

## Recipe 3: Knowledge Base Synthesis

**Goal:** Create new pages from existing related content.

**Workflow:**
1. Agent gathers 3–5 related documents on a topic.
2. Agent proposes a new synthesized page (outline + draft).
3. Human reviews and edits.
4. Publish new page under `/docs/` and link it from an index.

**Why it works:**  
Agents can stitch together context quickly; humans control narrative and accuracy.

---

## Recipe 4: Glossary Enrichment

**Goal:** Keep a glossary in sync with the docs.

**Workflow:**
1. Agent scans new/updated docs for terms.
2. Agent proposes glossary entries.
3. Human approves.
4. Publish glossary as a new version.

**Pattern:**  
`/docs/glossary.md` (append-only updates via new versions)

**Why it works:**  
Glossaries improve search and comprehension for both humans and agents.

---

## Recipe 5: MCP‑Driven Edits with Guardrails

**Goal:** Allow agents to propose edits while preserving human control.

**Workflow:**
1. MCP agent fetches content and suggests changes.
2. Human reviews suggested patch.
3. Human publishes approved change.
4. Server stores new immutable version.

**Why it works:**  
Keeps all changes reviewable and auditable without blocking agent assistance.

---

## Recipe 6: Versioned Correction Loop

**Goal:** Fix mistakes without rewriting history.

**Workflow:**
1. Agent detects an error (or inconsistency).
2. Agent proposes corrected text.
3. Human approves.
4. Publish as a new version, never overwriting history.

**Why it works:**  
Maintains integrity and auditability.

---

## Agent-to-Agent Workflows

Demarkus also supports agent‑to‑agent collaboration through the same read/write protocol:

- **Shared context**: one agent publishes structured notes or intermediate results; another agent reads and builds on them.
- **Work delegation**: an indexing agent publishes link maps or tag files; a writing agent uses them to generate drafts.
- **Handoff logs**: agents publish handoff summaries after tasks, keeping progress visible and auditable.
- **Queue documents**: use a shared task queue for handoffs and coordination ([queue template](#queue-template)).
- **Review gates**: publishing can still be restricted by tokens so agent outputs are reviewed before human‑visible updates.

Because all writes are versioned, every agent contribution is traceable and reversible by publishing a new version.

## Queue Template

Use a shared queue doc (e.g. `/docs/queue.md`) to coordinate tasks and handoffs:

```markdown
# Agent Queue

## New
- [ ] Task: Draft summary for /docs/architecture/index.md
  - Owner: agent-a
  - Source: /docs/architecture/index.md
  - Output: /docs/architecture/summary.md

## In Progress
- [ ] Task: Validate links under /docs/site/
  - Owner: agent-b

## Done
- [x] Task: Update /docs/index.md navigation
  - Owner: agent-c
```

## Best Practices

- **Use separate summary files** instead of modifying originals.
- **Prefer small, frequent publishes** to keep history clear.
- **Keep index pages updated** whenever new docs are added.
- **Use link graphs** to monitor coverage and discover gaps.
- **Require human review** for externally facing content.

---

## Related

- [Philosophy & Intent](index.md)
- [Why Demarkus](why-demarkus.md)
- [Architecture & Design](../architecture/index.md)
- [Docs Home](../index.md)