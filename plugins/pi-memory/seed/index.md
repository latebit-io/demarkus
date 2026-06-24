# Projects

Personal memory for coding agents, organized by project. The top-level `/index.md` is a project list. Each project lives in its own subdirectory with a consistent internal structure.

## Project List

_No projects yet. Add a link below when the agent creates a project subtree._

## Per-Project Structure

Each `/<project>/` subtree follows the canonical layout defined in **[/project-template.md](/project-template.md)** — the single source of truth. In brief: a maintained `index.md` hub, plus `architecture.md`, `patterns.md`, `guidelines.md`, `debugging.md`, `roadmap.md`, `debt.md`, `thoughts.md`, an `adr/` directory (one ADR per decision), a `plans/` directory, and `journal/<YYYY-MM-DD>.md` dated notes.

The agent keeps this layout consistent across projects so `/soul`, `/soul-journal`, and the memory skill always know where to look. See `/project-template.md` for the full descriptions and conventions.

## Technical

- Soul directory, port, and mode are in `~/.demarkus/plugin-memory.conf` (`SOUL_DIR`, `PORT`, `MODE`). Mode is `default` (shared at `~/.demarkus/soul/` on 6310), `isolated` (separate instance on 16310+), or `reuse` (adopting an existing `demarkus-server`).
- The plugin's auth token is at `~/.demarkus/plugin-memory.token` (mode 0600).
- Browse the same soul from the demarkus TUI using the port from the config: `demarkus-tui -host mark://localhost:$PORT`.
- Run `/soul-init` to reconfigure (switch modes, adopt an existing server, etc.).
