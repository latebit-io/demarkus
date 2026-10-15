package marktools

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/journal"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// ContextLeaf is the pseudo document under any directory that reads as a
// session primer: the hub, the newest journal entries and the active work.
// Reserved: a publish or append to it is refused.
const ContextLeaf = ".context"

const (
	contextJournalDays = 3
	contextBodyCap     = 4000
	contextPlanCap     = 20
	contextListCalls   = 50
)

// Context renders the primer for the directory the URL names (a store root
// or a project). Read only; what could not be read is listed under Coverage.
func (t *Tools) Context(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	dir := strings.TrimSuffix(target.Path, ContextLeaf)
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	p := primer{tools: t, host: target.Host, token: t.readToken(ctx, target.Host)}
	l, err := t.layoutFor(ctx, target)
	if err != nil {
		p.coverage = append(p.coverage, err.Error()+" (default layout assumed)")
	}
	journalHeading := layout.Section(l.Journal)
	fmt.Fprintf(&p.out, "# Context: %s\n", dir)
	switch p.section(ctx, "Hub", dir+"index.md", false) {
	case protocol.StatusNotFound:
		return text(fmt.Sprintf("status: new\n\nNo hub at %sindex.md: a new project; the journal route creates it.\n", dir))
	case protocol.StatusArchived:
		return text(fmt.Sprintf("status: archived\n\n%sindex.md is archived: a retired project.\n", dir))
	}
	entries, listed := p.newest(ctx, dir+l.Journal, contextJournalDays, journal.IsEntry)
	if listed && len(entries) == 0 {
		fmt.Fprintf(&p.out, "\n## %s\n\nNo entries yet.\n", journalHeading)
	}
	for _, e := range entries {
		p.section(ctx, journalHeading+" "+strings.TrimSuffix(path.Base(e), ".md"), e, false)
	}
	p.section(ctx, "Active work", dir+l.Roadmap, true)
	if plans, _ := p.newest(ctx, dir+l.Plans, contextPlanCap, nil); len(plans) > 0 {
		p.out.WriteString("\n## Plans\n\n")
		slices.Reverse(plans)
		for _, e := range plans {
			fmt.Fprintf(&p.out, "- %s\n", e)
		}
	}
	if len(p.coverage) > 0 {
		p.out.WriteString("\n## Coverage\n\n")
		for _, c := range p.coverage {
			fmt.Fprintf(&p.out, "- %s\n", c)
		}
	}
	return text(p.out.String())
}

// layoutFor reads the target's world layout; the Default comes back with the
// error so a reader may still render, while a writer must not.
func (t *Tools) layoutFor(ctx context.Context, target Target) (layout.Layout, error) {
	l, err := layout.Read(ctx, func(ctx context.Context, p string) (protocol.Response, error) {
		r, err := t.fetch(ctx, target, p)
		return r.Response, err
	})
	if err != nil {
		return layout.Default(), err
	}
	return l, nil
}

// primer renders one context read: sections in, gaps under coverage.
type primer struct {
	tools       *Tools
	host, token string
	out         strings.Builder
	coverage    []string
}

// section reads one document into the primer and reports its status ("" on
// a transport error); an optional one may be absent.
func (p *primer) section(ctx context.Context, heading, docPath string, optional bool) string {
	r, err := p.tools.backend.Fetch(ctx, fetch.FetchRequest{Host: p.host, Path: docPath, Token: p.token})
	switch {
	case err != nil:
		p.coverage = append(p.coverage, fmt.Sprintf("%s: %v", docPath, err))
		return ""
	case r.Response.Status == protocol.StatusNotFound && optional:
	case r.Response.Status != protocol.StatusOK:
		p.coverage = append(p.coverage, fmt.Sprintf("%s: %s", docPath, r.Response.Status))
	default:
		fmt.Fprintf(&p.out, "\n## %s (%s)\n\n%s\n", heading, docPath, clip(r.Response.Body, contextBodyCap, fmt.Sprintf("(truncated; mark_fetch %s with force: true for the rest)", docPath)))
	}
	return r.Response.Status
}

// newest lists one directory and returns up to n document paths the keep
// filter admits (nil keeps all), newest name first; dated journal names sort
// by day. A missing directory is empty; a failed listing is not listed.
func (p *primer) newest(ctx context.Context, dir string, n int, keep func(name string) bool) (paths []string, listed bool) {
	w := listwalk.Walker{
		Client: p.tools.backend, Host: p.host, Token: p.token, MaxDepth: listwalk.RootOnly, MaxLists: contextListCalls,
		OnProblem: func(problem *listwalk.Problem) error {
			if problem.Kind == listwalk.ProblemStatus && problem.Status == protocol.StatusNotFound {
				return nil
			}
			return problem
		},
	}
	err := w.Walk(ctx, dir, func(docPath string) error {
		if keep == nil || keep(path.Base(docPath)) {
			paths = append(paths, docPath)
		}
		return nil
	})
	if err != nil {
		p.coverage = append(p.coverage, fmt.Sprintf("%s: %v", dir, err))
		return nil, false
	}
	slices.Sort(paths)
	slices.Reverse(paths)
	if len(paths) > n {
		paths = paths[:n]
	}
	return paths, true
}

// clip bounds one body at a line, else a character, and ends it with the
// trailer when it was cut.
func clip(body string, limit int, trailer string) string {
	body = strings.TrimRight(body, "\n")
	if len(body) <= limit {
		return body
	}
	cut := limit
	for cut > 0 && body[cut-1] != '\n' {
		cut--
	}
	if cut == 0 {
		cut = limit
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
	}
	return strings.TrimRight(body[:cut], "\n") + "\n\n" + trailer
}

// isContextPath reports the primer pseudo document.
func isContextPath(p string) bool { return path.Base(p) == ContextLeaf }
