package guidance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
)

// Project states the probe reports. A failed probe is unknown, never new: it
// cannot tell an empty soul from one it did not reach.
const (
	ProjectKnown   = "known"
	ProjectNew     = "new"
	ProjectUnknown = "unknown"
)

const (
	probeDial      = time.Second
	probeRequest   = 2 * time.Second
	probeJournalLs = 20 // LIST pages for one journal directory
)

// ProjectStatus is what session start learned about the project in its soul.
type ProjectStatus struct {
	State       string // ProjectKnown, ProjectNew or ProjectUnknown
	Reason      string // ProjectUnknown only
	Root        string // project directory; the default root when the template is unread
	LastJournal string // newest entry's date; "" when none or the listing failed
	JournalPath string // newest entry's path, set with LastJournal
	NoJournal   bool   // the journal listed and holds no entry
}

// Probe reads the project's state in its store within the session budget.
func Probe(store, slug string) ProjectStatus {
	ctx, cancel := context.WithTimeout(context.Background(), statusBudget)
	defer cancel()
	return probeProject(ctx, store, slug)
}

// soulReader is the part of the protocol client the probe uses.
type soulReader interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
}

// soul is one store reached through a reader.
type soul struct {
	reader      soulReader
	host, token string
}

func (s soul) fetch(ctx context.Context, p string) (protocol.Response, error) {
	r, err := s.reader.Fetch(ctx, fetch.FetchRequest{Host: s.host, Path: p, Token: s.token})
	return r.Response, err
}

// errHosted marks a broker store: it needs the harness's OAuth session, so
// session start does not probe it.
var errHosted = errors.New("hosted store, not checked")

// openSoul dials the catalog store; the caller closes it.
func openSoul(store string) (soul, func(), error) {
	ep, err := catalog.Resolve(store)
	if err != nil {
		return soul{}, nil, err
	}
	if ep.Broker {
		return soul{}, nil, errHosted
	}
	host, err := links.DialHost(ep.Host)
	if err != nil {
		return soul{}, nil, fmt.Errorf("store host %s: %w", ep.Host, err)
	}
	c := fetch.NewClient(fetch.Options{Insecure: ep.Insecure, DialTimeout: probeDial, RequestTimeout: probeRequest, KeepAlivePeriod: -1})
	return soul{reader: c, host: host, token: ep.Token}, func() { c.Close() }, nil
}

// probeStore probes the catalog store for the project.
func probeStore(ctx context.Context, store, slug string) ProjectStatus {
	s, done, err := openSoul(store)
	if err != nil {
		return unknownStatus(slug, err.Error())
	}
	defer done()
	return probe(ctx, s, slug)
}

// earlierStore is the root in store holding a live hub for slug, "" for none:
// where a checkout wrote while its directory name was its project.
func earlierStore(ctx context.Context, store, slug string) (string, error) {
	s, done, err := openSoul(store)
	if errors.Is(err, errHosted) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer done()
	l, err := layout.Read(ctx, s.fetch)
	if err != nil {
		return "", err
	}
	root := l.Root(slug)
	// An archived hub is a root already retired: only an OK one counts.
	for _, p := range hub.Paths(root) {
		resp, err := s.fetch(ctx, p)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", p, err)
		}
		if resp.Status == protocol.StatusOK {
			return root, nil
		}
		if resp.Status != protocol.StatusNotFound {
			break
		}
	}
	return "", nil
}

// probe reads the template and the default hub together. A template naming
// another root cancels the default hub read, so it cannot eat the root's time.
func probe(ctx context.Context, s soul, slug string) ProjectStatus {
	st := unknownStatus(slug, "")
	type fetched struct {
		resp protocol.Response
		err  error
	}
	hubCtx, cancelHub := context.WithCancel(ctx)
	defer cancelHub()
	hubDone := make(chan fetched, 1)
	defaultHub := st.Root + "index.md" // read before st.Root may change
	go func() {
		resp, err := s.fetch(hubCtx, defaultHub)
		hubDone <- fetched{resp, err}
	}()
	l, lErr := layout.Read(ctx, s.fetch)
	if lErr != nil {
		// The default root would call the root project new.
		st.Reason = failure(ctx, lErr)
		return st
	}
	var resp protocol.Response
	var hErr error
	if root := l.Root(slug); root != st.Root {
		st.Root = root
		cancelHub()
		resp, hErr = s.fetch(ctx, root+"index.md")
	} else {
		r := <-hubDone
		resp, hErr = r.resp, r.err
	}
	hubPath := st.Root + "index.md"
	// Without an index.md, the project's hub may be its <dir>.md.
	if paths := hub.Paths(st.Root); hErr == nil && resp.Status == protocol.StatusNotFound && len(paths) > 1 {
		hubPath = paths[1]
		resp, hErr = s.fetch(ctx, hubPath)
	}
	switch {
	case hErr != nil:
		st.Reason = fmt.Sprintf("fetch %s: %s", hubPath, failure(ctx, hErr))
		return st
	case resp.Status == protocol.StatusNotFound:
		st.State = ProjectNew
		return st
	case resp.Status != protocol.StatusOK && resp.Status != protocol.StatusArchived:
		st.Reason = hubPath + ": " + resp.Status
		return st
	}
	st.State, st.Reason = ProjectKnown, ""
	dir := st.Root + l.Journal
	name, none := newestEntry(ctx, s, dir)
	if name != "" {
		st.LastJournal, st.JournalPath = strings.TrimSuffix(name, ".md"), dir+name
	}
	st.NoJournal = none
	return st
}

// newestEntry is the newest dated entry's name under dir. A missing directory
// has none; a failed listing is logged and leaves both results empty.
func newestEntry(ctx context.Context, s soul, dir string) (name string, none bool) {
	w := listwalk.Walker{
		Client: s.reader, Host: s.host, Token: s.token, MaxDepth: listwalk.RootOnly, MaxLists: probeJournalLs,
		// A missing directory has no entries; subdirectories hold none.
		OnProblem: func(p *listwalk.Problem) error {
			if p.Kind == listwalk.ProblemDepth || (p.Kind == listwalk.ProblemStatus && p.Status == protocol.StatusNotFound) {
				return nil
			}
			return p
		},
	}
	var newest string
	err := w.Walk(ctx, dir, func(docPath string) error {
		if name := path.Base(docPath); layout.IsJournalEntry(name) && name > newest {
			newest = name
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] guidance: newest journal entry: "+failure(ctx, err))
		return "", false
	}
	return newest, newest == ""
}

func unknownStatus(slug, reason string) ProjectStatus {
	def := layout.Default()
	return ProjectStatus{State: ProjectUnknown, Reason: reason, Root: def.Root(slug)}
}

// failure words an error for the status line; the deadline is the common one.
func failure(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "no answer within " + statusBudget.String()
	}
	return err.Error()
}
