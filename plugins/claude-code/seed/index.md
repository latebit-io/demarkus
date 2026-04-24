# Projects

Personal memory for Claude Code agents, organized by project. The top-level `/index.md` is a project list. Each project lives in its own subdirectory with a consistent internal structure.

## Project List

_No projects yet. Add a link below when the agent creates a project subtree._

## Per-Project Structure

Each `/<project>/` subtree contains:

- `plan/tasks.md` — active work, priorities, what's in flight
- `architecture.md` — system design, module boundaries, key decisions
- `patterns.md` — code patterns, conventions, idioms
- `roadmap.md` — what's done, what's next, what's deliberately not prioritized
- `adr/` — Architecture Decision Records, one file per decision (e.g. `0001-auth-model.md`)
- `journal/` — dated session notes, one file per day (`YYYY-MM-DD.md`), appended as the day progresses

The agent keeps this layout consistent across projects so `/soul`, `/soul-journal`, and the memory skill know where to look.

## Technical

- Soul directory, port, and mode are in `~/.demarkus/plugin-memory.conf` (`SOUL_DIR`, `PORT`, `MODE`). Mode is `default` (shared at `~/.demarkus/soul/` on 6310), `isolated` (separate instance on 16310+), or `reuse` (adopting an existing `demarkus-server`).
- The plugin's auth token is at `~/.demarkus/plugin-memory.token` (mode 0600).
- Browse the same soul from the demarkus TUI using the port from the config: `demarkus-tui -host mark://localhost:$PORT`.
- Run `/soul-init` in Claude Code to reconfigure (switch modes, adopt an existing server, etc.).
