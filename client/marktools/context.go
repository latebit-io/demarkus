package marktools

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/primer"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/docframe"
	"github.com/latebit-io/demarkus/protocol/layout"
)

// ContextLeaf is the pseudo document under any directory that reads as a
// session primer: the hub, the newest journal entries and the active work.
// Reserved: a publish or append to it is refused.
const ContextLeaf = primer.Leaf

// Context renders the primer (client/memory/primer) for the directory the
// URL names: a store root or a project. Read only; what could not be read is
// listed under Coverage.
func (t *Tools) Context(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	dir := strings.TrimSuffix(target.Path, ContextLeaf)
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	token := t.readToken(ctx, target.Host)
	var coverage []string
	l, err := t.layoutFor(ctx, target)
	if err != nil {
		coverage = append(coverage, err.Error()+" (default layout assumed)")
	}
	p := primer.Read(ctx, &primer.Request{
		Dir: dir, Layout: l,
		Fetch: func(ctx context.Context, docPath string) (protocol.Response, error) {
			r, err := t.backend.Fetch(ctx, fetch.FetchRequest{Host: target.Host, Path: docPath, Token: token})
			return r.Response, err
		},
		Walker: listwalk.Walker{Client: t.backend, Host: target.Host, Token: token},
	})
	switch p.State {
	case primer.New:
		return text(fmt.Sprintf("status: new\n\nNo hub at %s: a new project; the journal route creates it.\n", strings.Join(p.HubPaths, " or ")))
	case primer.Archived:
		return text(fmt.Sprintf("status: archived\n\n%s is archived: a retired project.\n", p.Hub))
	}
	p.Coverage = append(coverage, p.Coverage...)
	return text(primerText(&p))
}

// primerText words a primer: each document framed as the untrusted text it is.
func primerText(p *primer.Primer) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Context: %s\n", p.Dir)
	section := func(s *primer.Section) {
		body := strings.TrimSuffix(docframe.Frame(s.Body)+docframe.Flags(s.Body, s.Metadata), "\n")
		if s.Cut {
			body += fmt.Sprintf("\n\n(truncated; mark_fetch %s with force: true for the rest)", s.Path)
		}
		fmt.Fprintf(&out, "\n## %s (%s)\n\n%s\n", s.Heading, s.Path, body)
	}
	for i := range p.Sections {
		section(&p.Sections[i])
	}
	if p.JournalEmpty {
		fmt.Fprintf(&out, "\n## %s\n\nNo entries yet.\n", p.JournalHeading)
	}
	if p.Active != nil {
		section(p.Active)
	}
	if len(p.Plans) > 0 {
		out.WriteString("\n## Plans\n\n")
		for _, e := range p.Plans {
			fmt.Fprintf(&out, "- %s\n", e)
		}
	}
	if len(p.Coverage) > 0 {
		out.WriteString("\n## Coverage\n\n")
		for _, c := range p.Coverage {
			fmt.Fprintf(&out, "- %s\n", c)
		}
	}
	return out.String()
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

// bound cuts a body shown in a report (primer.Bound).
func bound(body string, limit int) (kept string, cut bool) { return primer.Bound(body, limit) }

// isContextPath reports the primer pseudo document.
func isContextPath(p string) bool { return primer.IsLeaf(p) }
