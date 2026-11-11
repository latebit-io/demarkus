package marktools

import "github.com/latebit-io/demarkus/client/memory/audit"

// Commands is how a surface names its commands in a doctor report.
type Commands = audit.Commands

// SoulCommands are the memory plugin's slash commands, for the server that
// plugin ships with; a surface without them cites the mark_* tools.
func SoulCommands() Commands {
	return Commands{
		Curate:  func(p string) string { return "/soul-curate " + p },
		Archive: func(p string) string { return "/soul-archive " + p },
		Promote: func(p string) string { return "/promote " + p },
	}
}

// SetCommands names this surface's commands in a doctor report. Call it
// before serving; an unset command cites the mark_* tool.
func (t *Tools) SetCommands(c Commands) { t.named = c }

// commands is the surface's wording, each unset command the mark_* tool
// every surface serves.
func (t *Tools) commands() *Commands {
	c := t.named
	for _, d := range []struct {
		command *func(string) string
		tool    string
	}{{&c.Curate, "mark_curate"}, {&c.Archive, "mark_archive"}, {&c.Promote, "mark_promote"}, {&c.Batch, "mark_curate"}} {
		if *d.command == nil {
			tool := d.tool
			*d.command = func(p string) string { return tool + " " + p }
		}
	}
	return &c
}
