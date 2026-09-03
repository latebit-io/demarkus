---
layout: default
title: Memory
permalink: /memory/
redirect_from:
  - /soul/
---

# Memory

The Demarkus project keeps a live memory: persistent, versioned memory for the AI agent that helps build it. It was formerly called the soul, and its hostname keeps that name.

This is an experiment in the **project memory pattern**: a minimal Demarkus server holding architecture notes, debugging lessons, a roadmap, a journal, and the agent's own thoughts. Each session, the agent reconnects, reads what it left behind, and picks up where it stopped.

<div class="memory-demo">
  <img src="/demarkus-memory.gif" alt="Claude Agent connecting to demarkus-memory via MCP" />
  <p class="memory-demo-caption">Claude Agent connecting to the memory via MCP</p>
</div>

The memory is served at `mark://soul.demarkus.io` and can be browsed with any Demarkus client, or read in the browser at **[soul.demarkus.io](https://soul.demarkus.io)** through the [library](/library/) reading room.

## Connect to the memory

### 1. Install the client

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  bash -s -- --client-only
```

### 2. Browse from the CLI

```bash
# Read the index
demarkus mark://soul.demarkus.io/index.md

# Read the agent's journal
demarkus mark://soul.demarkus.io/journal.md

# Read the agent's thoughts
demarkus mark://soul.demarkus.io/thoughts.md

# Discover what's available
demarkus info mark://soul.demarkus.io
```

Or use the TUI for an interactive experience:

```bash
demarkus-tui mark://soul.demarkus.io/index.md
```

### 3. Connect via MCP

Agents can connect to the memory using `demarkus-mcp`. Add this to your `.mcp.json`:

```json
{
  "mcpServers": {
    "demarkus-memory": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://soul.demarkus.io"
      ]
    }
  }
}
```

Fifteen MCP tools: `mark_fetch`, `mark_list`, `mark_versions`, `mark_lookup`, `mark_explore`, `mark_publish`, `mark_append`, `mark_archive`, `mark_resolve`, `mark_index`, `mark_discover`, `mark_graph`, `mark_backlinks`, `mark_graph_export`, `mark_graph_publish`. `mark_lookup` is the card catalog (subject → documents, ranked by importance), `mark_explore` orients you in one call, `mark_backlinks` finds what links to a page, and `mark_graph_publish` shares your crawled topology with other agents. A broker-fronted [knowledge system](/scenarios/knowledge-system/) adds two more, `mark_worlds` and `mark_lookup_all`, for seventeen in total.

`mark_fetch` is size-adaptive: documents under 8KB return whole, larger ones return an outline of headings with `#anchors`, so you fetch `path.md#section` instead of the entire file.

The server also vends **resources** (`mark://{host}/{+path}`, attachable in the client's picker, `#anchor` for a single section) and three **prompts** (`orient`, `recall`, `whats-new`) as slash-style commands in MCP clients that support them.

## What's on the memory

| Document | Contents |
|---|---|
| `index.md` | Hub page linking to all sections |
| `architecture.md` | System design, module boundaries, key decisions |
| `patterns.md` | Code patterns, build commands, conventions |
| `guidelines.md` | Hard code-quality rules, read before writing code |
| `conventions.md` | Collaboration and repo/plugin working agreements |
| `debugging.md` | Lessons learned from bugs and investigations |
| `roadmap.md` | What's done and what's next |
| `debt.md` | Technical debt and improvement opportunities |
| `journal/<YYYY-MM-DD>.md` | Dated session notes, one file per day |
| `thoughts.md` | The agent's own reflections |
| `guide.md` | Setup instructions for the memory pattern |
| `universe.md` | Memories, worlds, and hubs as a deployment topology |
| `plans/<name>.md` | Plan documents, active and archived |
| `faq.md` | Common questions and comparisons |

All documents are public and read-open. The version history of every page is permanent, so you can fetch any past version.

## Browse it in the library

This memory is served through the [library](/library/) at
**[soul.demarkus.io](https://soul.demarkus.io)**: read it in the browser, walk
the trail, and open the floor and the document graph, no account needed.

The library defaults to this world, so a local run gives you the same reading
room over your own copy:

```bash
git clone https://github.com/latebit-io/demarkus-library
cd demarkus-library && go run ./cmd/demarkus-library
```

## Run your own memory

Want persistent memory for your own AI agent? See the [Agent Memory scenario](/scenarios/agent-memory/) for a step-by-step guide.
