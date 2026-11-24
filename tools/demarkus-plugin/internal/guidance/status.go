package guidance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

// statusBudget bounds the probe and the git read together; session start
// waits on both. earlierBudget is the hint's share: a store that is down
// must not hold every start to the full budget for an aid.
const (
	statusBudget  = 3 * time.Second
	earlierBudget = 1500 * time.Millisecond
)

const standsHeading = "Where it stands:"

// probeProject and probeEarlier are the store probes; tests replace them so
// none dials out.
var (
	probeProject = probeStore
	probeEarlier = earlierStore
)

// earlierRoot is where the directory-name rule kept this checkout's notes,
// when they are still there and it resolves elsewhere now.
type earlierRoot struct{ Store, Root, Slug string }

// sessionStatus is the project probe and the git facts, read side by side.
type sessionStatus struct {
	project ProjectStatus
	earlier *earlierRoot
	git     *facts.Git // nil: no git facts, gitNote says why
	gitNote string
}

func readStatus(r project.Resolution) sessionStatus {
	ctx, cancel := context.WithTimeout(context.Background(), statusBudget)
	defer cancel()
	var s sessionStatus
	var wg sync.WaitGroup
	wg.Go(func() { s.project = probeProject(ctx, r.Store, r.Slug) })
	if store, slug, moved := formerly(r); moved {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, earlierBudget)
			defer cancel()
			root, err := probeEarlier(ctx, store, slug)
			if err != nil {
				// The hint is an aid; the session starts without it.
				fmt.Fprintln(os.Stderr, "[demarkus-plugin] guidance: earlier root in "+store+": "+err.Error())
			}
			if root != "" {
				s.earlier = &earlierRoot{Store: store, Root: root, Slug: slug}
			}
		})
	}
	wg.Go(func() {
		g, err := facts.ReadGit(ctx, r.Dir)
		if err != nil {
			s.gitNote = gitFailure(ctx, err)
			return
		}
		s.git = &g
	})
	wg.Wait()
	if s.earlier != nil && s.earlier.Store == r.Store && s.earlier.Root == s.project.Root {
		s.earlier = nil
	}
	return s
}

// formerly is the store and slug the directory-name rule gave r. moved is
// false when r still resolves there, or that store was never set up.
func formerly(r project.Resolution) (store, slug string, moved bool) {
	slug, store, err := project.ByDirName(r.Dir)
	if errors.As(err, new(*project.SlugError)) {
		return "", "", false // the old rule named no project here
	}
	if store == "" && err == nil {
		var configured bool
		if configured, err = LocalConfigured(); err == nil && !configured {
			return "", "", false
		}
		store = config.LocalMemoryID
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] guidance: earlier root: "+err.Error())
		return "", "", false
	}
	return store, slug, store != r.Store || slug != r.Slug
}

// sentence tells the agent the earlier root is for recall only. A renamed
// clone can keep it; a worktree cannot, its config is the repository's.
func (e *earlierRoot) sentence(r project.Resolution) string {
	s := " Earlier root: this checkout's older notes are under `" + e.Root + "` in store `" + e.Store +
		"`; recall there too, write only under `Project root:`."
	if !r.Linked && r.Source == project.SourceOrigin {
		s += " To keep that root instead: `git config demarkus.project " + e.Slug + "`."
	}
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

// agentHeader is the project line, its state, the verdict a prompt acts on,
// and where the work stands. The local store is configured by the time a
// header is built, so the verdict needs no second check.
func (s *sessionStatus) agentHeader(r project.Resolution) string {
	stands := standsHeading + " no git facts (" + s.gitNote + ")."
	if s.git != nil {
		stands = strings.TrimSuffix(facts.Render(standsHeading, facts.Session{Git: s.git}), "\n")
	}
	earlier := ""
	if s.earlier != nil {
		earlier = s.earlier.sentence(r)
	}
	return header(r, s.project.Root) + " " + s.project.sentence() + earlier + " Verdict: " + Verdict(r, s.project, true) + "\n\n" + stands
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
func (s *sessionStatus) userLine(r project.Resolution, warn string, brand config.Brand) string {
	line := brand.PluginName + ": " + brand.Store + " " + r.Store
	if r.Inherited {
		line += " (main checkout's)"
	}
	line += " · project " + r.Slug
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
func slugStatus(err *project.SlugError, brand config.Brand) string {
	return fmt.Sprintf("%s: no project (directory name %q is not a valid slug)", brand.PluginName, err.Name)
}

// identityStatus is the user line for a checkout git's files cannot name.
func identityStatus(err *project.IdentityError, brand config.Brand) string {
	return brand.PluginName + ": no project (" + err.Reason + ")"
}
