package marktools

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
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
	fmt.Fprintf(&p.out, "# Context: %s\n", dir)
	p.section(ctx, "Hub", dir+"index.md", false)
	entries, listed := p.newest(ctx, dir+"journal/", contextJournalDays, journalDay)
	if listed && len(entries) == 0 {
		p.out.WriteString("\n## Journal\n\nNo entries yet.\n")
	}
	for _, e := range entries {
		p.section(ctx, "Journal "+strings.TrimSuffix(path.Base(e), ".md"), e, false)
	}
	p.section(ctx, "Active work", dir+"roadmap.md", true)
	if plans, _ := p.newest(ctx, dir+"plans/", contextPlanCap, nil); len(plans) > 0 {
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

// primer renders one context read: sections in, gaps under coverage.
type primer struct {
	tools       *Tools
	host, token string
	out         strings.Builder
	coverage    []string
}

// section reads one document into the primer; an optional one may be absent.
func (p *primer) section(ctx context.Context, heading, docPath string, optional bool) {
	r, err := p.tools.backend.Fetch(ctx, fetch.FetchRequest{Host: p.host, Path: docPath, Token: p.token})
	switch {
	case err != nil:
		p.coverage = append(p.coverage, fmt.Sprintf("%s: %v", docPath, err))
	case r.Response.Status == protocol.StatusNotFound && optional:
	case r.Response.Status != protocol.StatusOK:
		p.coverage = append(p.coverage, fmt.Sprintf("%s: %s", docPath, r.Response.Status))
	default:
		fmt.Fprintf(&p.out, "\n## %s (%s)\n\n%s\n", heading, docPath, clip(r.Response.Body, docPath))
	}
}

// journalDay keeps the dated entries of a journal directory.
var journalDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.md$`)

// newest lists one directory and returns up to n document paths the keep
// filter admits (nil keeps all), newest name first; dated journal names sort
// by day. A missing directory is empty; a failed listing is not listed.
func (p *primer) newest(ctx context.Context, dir string, n int, keep *regexp.Regexp) (paths []string, listed bool) {
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
		if keep == nil || keep.MatchString(path.Base(docPath)) {
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

// clip bounds one body in the primer; the rest is one fetch away.
func clip(body, docPath string) string {
	body = strings.TrimRight(body, "\n")
	if len(body) <= contextBodyCap {
		return body
	}
	cut := contextBodyCap
	for cut > 0 && body[cut-1] != '\n' {
		cut--
	}
	if cut == 0 {
		cut = contextBodyCap
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
	}
	return strings.TrimRight(body[:cut], "\n") + fmt.Sprintf("\n\n(truncated; mark_fetch %s with force: true for the rest)", docPath)
}

// isContextPath reports the primer pseudo document.
func isContextPath(p string) bool { return path.Base(p) == ContextLeaf }
