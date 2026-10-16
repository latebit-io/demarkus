---
layout: default
title: Agent Memory
permalink: /scenarios/agent-memory/
---

# Agent Memory

Give Claude Code, Cursor, OpenCode, pi, or any MCP-compatible LLM agent persistent memory across sessions: architecture notes, debugging lessons, roadmap, journal.

This is the pattern used by the Demarkus project itself. You can browse the live example at `mark://soul.demarkus.io`.

## What you'll have

- A Demarkus server holding structured markdown docs
- MCP tools for lookup, reading, writing, graph traversal, and curation. The exposed set depends on the tool profile; a [knowledge system](/scenarios/knowledge-system/) adds world discovery and system-wide lookup
- Version history of every memory update
- Persistent document graph with backlink queries
- The agent reads context at session start and writes updates at the end

## Quick start: the plugins

Claude Code, [Cursor](https://cursor.com), [OpenCode](https://opencode.ai), and
[pi](https://pi.dev) each have a plugin, with the same behavior mapped onto each
agent's extension API. They share `~/.demarkus` state, so one machine running all
four has one memory, one token, one registry.

### Claude Code

`demarkus-memory` is the one-step path, with no manual server, token, or MCP config to set up. Install it from the marketplace:

```
/plugin marketplace add latebit-io/demarkus
/plugin install demarkus-memory@demarkus
```

On the first session it spawns a local `demarkus-server`, generates a publish token, wires the MCP tools, and **seeds the memory**. Your memory is also called a soul: `/soul-context`, `/soul-journal`, `/soul-status`, `/soul-doctor`, `/soul-curate`, and `/soul-archive` work on it. The `remember` skill handles recall and recording.

Hooks keep the catalog honest: a publish without tags is caught at write time, an end-of-session nudge asks for a journal entry when files changed but nothing was recorded, and a "did we decide X" question nudges a recall before you answer from context.

Memory is organized **per project** under `/<project>/`, following the store's `/.well-known/demarkus/template.md`. A declared root project uses `/`. The agent maintains the project hub as it adds documents.

### OpenCode

Install `demarkus-opencode-memory` with the standalone installer:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory/install.sh | bash
```

Restart OpenCode. The first session provisions the memory. `/soul-status`
diagnoses the connection; `/soul-init` reconfigures it.

### pi

`demarkus-pi-memory` is the same plugin against pi's extension API. It needs the
MCP adapter for the tool surface:

```bash
pi install npm:pi-mcp-adapter
```

Install from the standalone mirror:

```bash
pi install git:github.com/latebit-io/demarkus-pi-memory
```

The memory provisions on the next session. Run `/mcp` once (or restart pi) to
connect the newly registered server; `/soul-status` diagnoses, `/soul-init`
reconfigures.

### Cursor

`demarkus-memory` for Cursor is a Cursor Plugin: bash hook shims over the same
`demarkus-plugin` binary, with the publish gate mapped onto `beforeMCPExecution`
(Cursor supports `ask` natively) and the journal nudge onto `stop`. Install it by
adding the repository as a marketplace from the plugins panel, then adding the
plugin:

```
Customize → Add Marketplace → Import from GitHub → https://github.com/latebit-io/demarkus (scope: User) → Import
demarkus-memory → Add
```

Enable **Auto Refresh** on the marketplace so updates follow the repository. Or
symlink a checkout into Cursor's local plugin directory:

```bash
git clone https://github.com/latebit-io/demarkus
ln -s "$PWD/demarkus/plugins/cursor-memory" ~/.cursor/plugins/local/demarkus-memory
```

Restart Cursor. The first conversation provisions the memory; if the
`demarkus-memory` MCP server shows as failed before that, toggle it once under
Settings → MCP. `/soul-status` diagnoses, `/soul-init` reconfigures. Cloud
agents run no session hooks and cannot reach a local server, so the plugin is
for the desktop IDE.

That is the whole setup for any of the four agents. The manual steps below are for other MCP agents, a custom port, or a remote memory server.

## Manual setup (any MCP agent)

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
```

On macOS, this installs the server and registers it as a launchd service.
On Linux, this installs and enables the systemd service.

### 2. Create the memory directory

```bash
mkdir -p ~/memory
```

That's it: leave it empty. You don't pre-build an index or a structure by hand. The agent creates and maintains the layout itself (driven by the `CLAUDE.md` instructions in step 6), publishing `index.md` and the per-project documents on its first writes. The server serves an empty root fine until then. See [Recommended memory structure](#recommended-memory-structure) for the layout the agent should follow.

### 3. Generate a publish token

```bash
demarkus-token generate -label my-memory -paths "/**" -ops publish -tokens ~/memory/tokens.toml
```

`-label` is required. A single `publish` op is all you need; it authorizes `PUBLISH`, `APPEND`, and `ARCHIVE` (there is no separate `archive` operation). Copy the raw token from the output, since you'll need it for the MCP config.

### 4. Start the memory server on port 6310

Run it alongside your main server (which uses 6309):

```bash
demarkus-server -root ~/memory -tokens ~/memory/tokens.toml -port 6310
```

The flag is `-port` (an integer), not `-addr`. Run this alongside any main server you already have on the default 6309.

### 5. Configure MCP for your project

Create or update `.mcp.json` in your project root:

```json
{
  "mcpServers": {
    "demarkus-memory": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://localhost:6310",
        "-token", "<your-publish-token>",
        "-insecure"
      ]
    }
  }
}
```

Replace `/path/to/demarkus-mcp` with the actual path (`which demarkus-mcp` to find it).

### 6. Add CLAUDE.md instructions

Tell the agent how to use the memory. Create `CLAUDE.md` in your project:

```markdown
# CLAUDE.md

## Memory

All project context lives on the memory server, organized per project under
`/<project>/`.

### Preflight (every session)

1. `mark_fetch` `/<project>/index.md` (the project hub)
2. `mark_fetch` `/<project>/patterns.md` and `/<project>/guidelines.md`
3. `mark_lookup` to find memories by subject; fetch other docs as needed

### During work

- `mark_append` for incremental notes; `mark_publish` when rewriting a section
- Always pass `expected_version` from a prior fetch (optimistic concurrency)
- Tag every publish with `metadata.tags` + `importance` so `mark_lookup`
  can find it; keep `index.md` pointing at new docs as a backstop

### End of session

- Append to today's journal at `/<project>/journal/<YYYY-MM-DD>.md`
```

### 7. Verify

Open a Claude Code session and run:

```
Please check mark://localhost:6310/health, then initialize this project's memory.
```

The agent should verify connectivity and create the project's hub and first notes.

## Graph exploration

Agents can map the document graph and query backlinks:

```
# Crawl links from a document (results are persisted locally)
mark_graph url="/index.md" depth=3

# Find what links to a specific page
mark_backlinks url="/architecture.md"

# Discover what a server offers
mark_discover
```

`mark_graph` crawls outbound links from a document, building a persistent graph stored at `~/.mark/graph.json`. Each crawl merges into the existing graph, so knowledge accumulates across sessions.

`mark_backlinks` queries reverse links with bounded source revalidation. Results carry revision and freshness information.

### Sharing the graph

The full tool profile can export and publish the graph as markdown:

```
# Export the graph as publishable markdown
mark_graph_export

# Export and publish in one step
mark_graph_publish url="/graphs/my-network.md" expected_version=0
```

The published graph contains `mark://` links that other agents can follow. See the [Public Hub](/scenarios/public-hub/) scenario.

## Recommended memory structure

Memory is organized **per project**. Each project lives under `/<project>/`
(the slug is the basename of your project directory, lowercased, spaces →
hyphens) and follows the canonical layout, the same one the plugin seeds as
`/.well-known/demarkus/template.md`:

```
/<project>/index.md              # the project hub; links to every doc below
/<project>/architecture.md       # system design, module boundaries, decisions
/<project>/patterns.md           # code patterns, conventions, idioms
/<project>/guidelines.md         # hard code-quality rules (read before coding)
/<project>/debugging.md          # lessons from bugs and investigations
/<project>/roadmap.md            # what's done, what's next, what's deferred
/<project>/debt.md               # technical debt and improvement opportunities
/<project>/thoughts.md           # open questions, reflections, undecided ideas
/<project>/adr/<NNNN>-<slug>.md  # one Architecture Decision Record per decision
/<project>/plans/<name>.md       # plan documents (carry lifecycle in the text)
/<project>/journal/<YYYY-MM-DD>.md # dated session notes, one file per day
```

Not every project needs every file. Create a doc when there's something real
to put in it. The common core is `index.md`, `journal/`, and whichever of
architecture / patterns / decisions the work actually produces. The agent keeps
the per-project `index.md` current as the discovery backstop for anything
`mark_lookup` can't surface.

> A single-project memory can instead keep these files flat at the root (no
> `/<project>/` prefix). That's what the Demarkus project's own memory at
> `mark://soul.demarkus.io` does, declared by `root_project` in its template.

## Remote memories and promotion

A memory does not have to be local. `/soul-join` takes a join URL (host and token
in one paste-able string), registers that server as an MCP server, and binds it to
the current project. `/soul-default` picks which joined memory a project writes to
when several exist, and the binding is enforced at write time rather than left to
convention. `demarkus-token join -host <host> -token <token>` builds a join URL.

When a note is ready for other people, `/promote <path>` lifts it to a joined
[knowledge system](/scenarios/knowledge-system/): the cascade distills it, strips
personal framing and secrets, dedups against the shared catalog, tags it to that
system's taxonomy, picks a writable world, and stops at a human gate before
publishing. `/promote-scan` finds candidates; `/soul-refresh` reconciles promoted
notes with the authoritative copy through review.

Memory is the draft tier, the knowledge system is the authoritative one. Nothing
auto-publishes. [How knowledge flows](/how-it-works/) walks the whole loop end to
end.

## Using a remote memory server manually

If you run the memory on a remote host with TLS, remove `-insecure` from the MCP args and use the `mark://` URL with your domain:

```json
{
  "mcpServers": {
    "demarkus-memory": {
      "command": "/path/to/demarkus-mcp",
      "args": [
        "-host", "mark://memory.yourdomain.com",
        "-token", "<your-token>"
      ]
    }
  }
}
```

## Live example

Browse the Demarkus project's own memory:

```bash
demarkus-tui mark://soul.demarkus.io/index.md
```

## Related

- [Memory page](/memory/)
- [Agent Install](/agent-install/)
- [Organizational knowledge system](/scenarios/knowledge-system/): where promoted notes land
- [The library](/library/): browse a world in the reading room
