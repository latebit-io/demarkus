package guidance

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

// statusBudget bounds the probe and the git read together; session start
// waits on both.
const statusBudget = 3 * time.Second

const standsHeading = "Where it stands:"

// probeProject is the store probe; tests replace it so none dials out.
var probeProject = probeStore

// sessionStatus is the project probe and the git facts, read side by side.
type sessionStatus struct {
	project ProjectStatus
	git     *facts.Git // nil: no git facts, gitNote says why
	gitNote string
}

func readStatus(r project.Resolution) sessionStatus {
	ctx, cancel := context.WithTimeout(context.Background(), statusBudget)
	defer cancel()
	var s sessionStatus
	var wg sync.WaitGroup
	wg.Go(func() { s.project = probeProject(ctx, r.Store, r.Slug) })
	wg.Go(func() {
		g, err := facts.ReadGit(ctx, r.Dir)
		if err != nil {
			s.gitNote = gitFailure(ctx, err)
			return
		}
		s.git = &g
	})
	wg.Wait()
	return s
}

// gitFailure words a failed git read; a directory outside any repository is
// the common, expected one.
func gitFailure(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "git did not answer within " + statusBudget.String()
	case errors.Is(err, exec.ErrNotFound):
		return "git not installed"
	case strings.Contains(err.Error(), "not a git repository"):
		return "not a git repository"
	}
	return err.Error()
}

// agentHeader is the project line, its state, and where the work stands.
func (s *sessionStatus) agentHeader(r project.Resolution) string {
	stands := standsHeading + " no git facts (" + s.gitNote + ")."
	if s.git != nil {
		stands = strings.TrimSuffix(facts.Render(standsHeading, facts.Session{Git: s.git}), "\n")
	}
	return r.Header(s.project.Root) + " " + s.project.sentence() + "\n\n" + stands
}

func (p *ProjectStatus) sentence() string {
	switch {
	case p.State == ProjectNew:
		return "Project: new, no hub at `" + p.Root + "index.md` yet."
	case p.State != ProjectKnown:
		return "Project: unknown (" + p.Reason + "). Treat recall misses as inconclusive."
	case p.LastJournal != "":
		return "Project: known, newest journal entry `" + p.JournalPath + "`."
	case p.NoJournal:
		return "Project: known, no journal entry yet."
	}
	return "Project: known."
}

// userLine is the one line the user sees. A server health warning replaces
// the journal and git tail; no paths.
func (s *sessionStatus) userLine(r project.Resolution, warn string) string {
	line := "demarkus: soul " + r.Store + " · project " + r.Slug
	switch s.project.State {
	case ProjectNew:
		line += " (new)"
	case ProjectUnknown:
		line += " (status unknown: " + s.project.Reason + ")"
	}
	if warn != "" {
		return line + " · ⚠️ " + warn
	}
	if s.project.LastJournal != "" {
		line += " · last journal " + s.project.LastJournal
	}
	if s.git != nil {
		line += " · " + s.git.Brief()
	}
	return line
}

// slugStatus is the user line for a directory name that is no project slug.
func slugStatus(err *project.SlugError) string {
	return fmt.Sprintf("demarkus: no project (directory name %q is not a valid slug)", err.Name)
}
