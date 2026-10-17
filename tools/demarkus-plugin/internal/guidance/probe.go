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
	"github.com/latebit-io/demarkus/client/hub"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/listwalk"
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

// Lines renders the status as key=value lines for the registry subcommand.
func (p *ProjectStatus) Lines() string {
	lines := "root=" + p.Root + "\nproject=" + p.State
	if p.State == ProjectUnknown {
		lines += "\nreason=" + p.Reason
	}
	return lines
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

// probeStore probes the catalog store for the project. Broker stores need the
// harness's OAuth session, so they are not probed.
func probeStore(ctx context.Context, store, slug string) ProjectStatus {
	ep, err := catalog.Resolve(store)
	if err != nil {
		return unknownStatus(slug, err.Error())
	}
	if ep.Broker {
		return unknownStatus(slug, "hosted store, not checked")
	}
	host, err := links.DialHost(ep.Host)
	if err != nil {
		return unknownStatus(slug, fmt.Sprintf("store host %s: %v", ep.Host, err))
	}
	c := fetch.NewClient(fetch.Options{Insecure: ep.Insecure, DialTimeout: probeDial, RequestTimeout: probeRequest, KeepAlivePeriod: -1})
	defer c.Close()
	return probe(ctx, soul{reader: c, host: host, token: ep.Token}, slug)
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
