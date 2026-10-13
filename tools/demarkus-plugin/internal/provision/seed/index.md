# Projects

Personal memory for Claude Code agents, organized by project. The top-level `/index.md` is a project list. Each project lives in its own subdirectory with a consistent internal structure.

## Project List

_No projects yet. Add a link below when the agent creates a project subtree._

## Per-Project Structure

Each `/<project>/` subtree follows the memory layout the remember skill lists: a maintained `index.md` hub plus the documents and the `adr/`, `plans/` and `journal/` directories it names.

The agent keeps this layout consistent across projects so `/soul`, `/soul-journal`, and the memory skill always know where to look. Directory names come from `/.well-known/demarkus/template.md` when published at the memory root.

## Technical

- Memory directory, port, and mode are in `~/.demarkus/plugin-memory.conf` (`SOUL_DIR`, `PORT`, `MODE`). Mode is `default` (shared at `~/.demarkus/soul/` on 6310), `isolated` (separate instance on 16310+), or `reuse` (adopting an existing `demarkus-server`).
- The plugin's auth token is at `~/.demarkus/plugin-memory.token` (mode 0600).
- Browse the same memory from the demarkus TUI using the port from the config: `demarkus-tui -host mark://localhost:$PORT`.
- Run `/soul-init` in Claude Code to reconfigure (switch modes, adopt an existing server, etc.).
