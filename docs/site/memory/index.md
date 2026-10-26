# Memory

Your agent's memory: versioned markdown it writes as it works and reads before the next task. Private by default. Your own memory store is your **soul**; the `/soul-*` commands act on it.

- **Records** decisions, bug lessons, patterns, and a daily journal, unprompted.
- **Recalls** before substantive work, by subject, returning the section that answers.
- **Versions** every change. Nothing is overwritten.
- **Links** notes into a [graph](../graph/index.md) the agent can walk.
- **Travels**: local, on your own server, or hosted. Export any time.

## Install

| Agent | Install |
|---|---|
| Claude Code | `/plugin marketplace add latebit-io/demarkus` then `/plugin install demarkus-memory@demarkus` |
| Claude Cowork | Same marketplace (Customize, Plugins), install `demarkus-memory` |
| Cursor | Customize → Add Marketplace → Import from GitHub → `https://github.com/latebit-io/demarkus` (scope: User) → `demarkus-memory` → Add |
| OpenCode | Installer below, then restart |
| pi | `pi install npm:pi-mcp-adapter` then `pi install git:github.com/latebit-io/demarkus-pi-memory` |

```bash
opencode plugin add 'github:latebit-io/demarkus#main::path:plugins/opencode-memory'
```

The first session starts a local server, mints a token, wires the tools, and seeds the memory with a layout. Every plugin shares `~/.demarkus`, so every agent on the machine shares one memory.

In Cowork the tools run on the host and hooks are sandboxed: gates and nudges stay quiet, and setup commands (`/soul-init`, `/soul-join`, `/promote`) run from a terminal.

## What you see

Each session opens with a status line and a primer: the bound soul, the project, the last journal entry, and where the branch stands.

```text
demarkus: soul <store> · project <slug> · last journal <date> · <branch state>
```

Then the agent files as it goes:

| What | Where |
|---|---|
| A decision | `/<project>/adr/<NNNN>-<title>.md` |
| A bug lesson | `/<project>/debugging.md` |
| A pattern | `/<project>/patterns.md` |
| Architecture, roadmap, debt, plans | `/<project>/<topic>.md` |
| Session progress | `/<project>/journal/<YYYY-MM-DD>.md` |

Each project keeps a hub at `/<project>/index.md`; new documents link from it on first publish. The store's `/.well-known/demarkus/template.md` declares the layout.

## Recall

- `mark_lookup` finds by tag and title, or section text with `match=body`. With a `budget` it returns the matching passages plus related documents.
- `mark_fetch` returns short documents whole and long ones as an outline; fetch `path.md#anchor` for one section.
- `mark_explore` orients around a document: outline, links, backlinks, siblings.
- `/<project>/.context` is a session primer: hub, newest journal entries, active work.

Ask "did we decide X?" and a nudge has the agent check memory before answering from context.

## Gates

Hooks and the store check every write. The refusal says what to fix.

| Gate | Default |
|---|---|
| Tags and importance | Warn |
| Writes to a soul other than the project's | Block |
| Style (one H1, summary, size) | Warn |
| Retention (prunes history) | Ask |
| Secrets | Always refused |
| Personal data | Warn |
| Near-duplicate | Refused, naming the existing document; override with a reason |

Files changed but nothing journaled? An end-of-session nudge asks for an entry.

## Commands

| Command | Does |
|---|---|
| `/soul-context` | Restore where you left off |
| `/soul-journal` | Append to today's journal |
| `/soul` | Show the project's index |
| `/soul-doctor` | Audit hygiene: broken links, orphans, untagged docs, duplicates |
| `/soul-curate` | Reshape a document, or split one that outgrew a fetch |
| `/soul-archive` | Archive a document or directory, history kept |
| `/soul-export` | Save every document verbatim to local files |
| `/soul-status` | Diagnose the connection |
| `/soul-init` | Configure the local soul |
| `/soul-join` | Add a remote or hosted soul and bind it to the project |
| `/soul-default` | Pick which soul this project writes to |
| `/promote` | Lift a note into a [knowledge base](../knowledge/index.md) |
| `/promote-scan` | Find notes worth promoting |
| `/soul-refresh` | Pull shared copies back down |

The `remember` skill handles "remember this" and "what do we know about X".

## Remote and hosted memory

A soul need not be local.

- **Your own server.** `/soul-join <join-url>` adds it and binds the project. `demarkus-token join` builds the URL.
- **Hosted.** A knowledge system can also serve hosted memory: sign in and get a private world, one per person. Connect any MCP host to its `/mcp` URL. See [Memory gateway](../tools/memory-broker.md).
- **Leave any time.** `/soul-export` (the `soul-export` prompt without the plugin) saves every document to local files.

## Live example

This project keeps its own soul at `mark://soul.demarkus.io`: architecture, ADRs, debugging lessons, plans, and a daily journal, written by the agent that helps build it. Open to read in the browser at [soul.demarkus.io](https://soul.demarkus.io), or with `demarkus-tui mark://soul.demarkus.io/index.md`.

## Any MCP agent

No plugin for your agent? Run a server and point `demarkus-mcp` at it.

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
mkdir -p ~/memory
demarkus-token generate -label my-memory -paths "/**" -ops publish -tokens ~/memory/tokens.toml
demarkus-server -root ~/memory -tokens ~/memory/tokens.toml -port 6310
```

```json
{
  "mcpServers": {
    "demarkus-memory": {
      "command": "/path/to/demarkus-mcp",
      "args": ["-host", "mark://localhost:6310", "-token", "<raw-token>", "-insecure"]
    }
  }
}
```

Leave the directory empty; the agent builds the layout. Tell it the rules in your agent instructions: recall at start, tag every publish, journal at the end. For a remote server with real TLS, drop `-insecure`.

With `-host` set, `demarkus-mcp` also serves the plugin's commands as MCP prompts of the same names: `soul`, `soul-context`, `soul-journal`, `soul-doctor`, `soul-curate`, `soul-archive`, `soul-export`, `promote`, `promote-scan` and `soul-refresh` (in Claude Desktop, the prompt picker). `promote` and `soul-refresh` reach a knowledge system added to the same host as its own MCP server.

## Related

- [Knowledge base](../knowledge/index.md)
- [Knowledge graph](../graph/index.md)
- [Glossary](../reference/glossary.md)
- [Docs Home](../index.md)
