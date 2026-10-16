package marktools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/audit"
	"github.com/latebit-io/demarkus/client/curate"
	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/protocol"
)

// curateSources bounds the linking documents read for anchored links.
const curateSources = 50

// CurateArgs are mark_curate's arguments: the document, and with Apply the
// proposal hash a report returned.
type CurateArgs struct {
	URL      string
	Apply    bool
	Proposal string
}

// Curate answers mark_curate: the findings and the proposed change, or with
// apply the writes that change makes. Apply is refused unless the proposal
// recomputed from the current head still hashes to what the caller saw.
func (t *Tools) Curate(ctx context.Context, args CurateArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	if args.Apply && args.Proposal == "" {
		return failure("apply needs the proposal hash from a prior report")
	}
	var write WriteFunc
	if args.Apply {
		if write, bad = t.writer(ctx, target, "publish"); bad != nil {
			return *bad
		}
	}
	if strings.HasSuffix(target.Path, "/") {
		return t.curateScope(ctx, target, args, write)
	}
	head, err := t.doc(ctx, target, nil).Head(ctx)
	if err != nil {
		return t.failed(SiteFetch, target.Host, err)
	}
	if head.Status != protocol.StatusOK {
		return failure("curate failed: fetch %s: %s", target.Path, head.Status)
	}
	proposal := curate.Propose(&curate.Input{Path: target.Path, Body: head.Body, Version: head.Version, Metadata: head.Metadata})
	if !args.Apply {
		var moves []curate.Move
		var notes []string
		if len(proposal.Moved) > 0 {
			inbound, n := t.anchoredInbound(ctx, target)
			moves, notes = proposal.Moves(inbound), n
		}
		return text(proposal.Report(moves, notes))
	}
	if len(proposal.Writes) > 0 && proposal.Hash != args.Proposal {
		return failure("proposal %s does not match the document at v%d (now %s); rerun without apply and review", args.Proposal, head.Version, proposal.Hash)
	}
	docs := func(p string) *docwrite.Doc { return t.doc(ctx, target.at(p), write) }
	applied, err := curate.Apply(ctx, docs, &proposal, t.identityMeta(ctx))
	var b strings.Builder
	for i := range applied {
		a := &applied[i]
		if a.Action == curate.ActionSkipped {
			fmt.Fprintf(&b, "- %s: skipped, identical\n", a.Path)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s v%d\n", a.Path, a.Action, a.Version)
		t.observeWrite(ctx, target.at(a.Path), graph.FetchResult{Status: protocol.StatusOK, Body: a.Body, Metadata: a.Metadata})
	}
	if err == nil {
		return text("status: applied\n" + b.String())
	}
	var refusal *curate.Refusal
	reason := err.Error()
	if !errors.As(err, &refusal) && len(applied) > 0 {
		reason = t.failed(SitePublish, target.Host, err).Text
	}
	if len(applied) == 0 {
		return failure("apply refused: %s", reason)
	}
	return failure("status: partial\n%s- %s\nThe writes above landed; fetch the document and rerun the report.", b.String(), reason)
}

// curateScope answers mark_curate on a directory: the doctor's fix plan for
// it, or with apply the batch written, refused unless the plan recomputed
// now still hashes to what the caller saw.
func (t *Tools) curateScope(ctx context.Context, target Target, args CurateArgs, write WriteFunc) Result {
	report, bad := t.runAudit(ctx, target, audit.Options{})
	if bad != nil {
		return *bad
	}
	plan := report.Plan
	if !args.Apply {
		return text(plan.Markdown(report.Scope))
	}
	if plan.Hash != args.Proposal {
		return failure("proposal %s does not match %s now (%s); rerun without apply and review", args.Proposal, report.Scope, cmp.Or(plan.Hash, "nothing to apply"))
	}
	docs := func(p string) *docwrite.Doc { return t.doc(ctx, target.at(p), write) }
	outcomes := audit.Apply(ctx, docs, plan, t.identityMeta(ctx))
	landed := 0
	for i := range outcomes {
		if o := &outcomes[i]; o.Result == audit.Applied {
			landed++
			t.observeWrite(ctx, target.at(o.Path), graph.FetchResult{Status: protocol.StatusOK, Body: o.Body, Metadata: o.Metadata})
		}
	}
	out := audit.ApplyMarkdown(report.Scope, plan, outcomes)
	if landed == 0 {
		return failure("%s", out)
	}
	return text(out)
}

// anchoredInbound reads the documents the graph store says link here and
// keeps the links that name a section. No store, or a source that could not
// be read, is a note: the list is then partial, never claimed complete.
func (t *Tools) anchoredInbound(ctx context.Context, target Target) (inbound []curate.Inbound, notes []string) {
	if t.hooks.Graph == nil {
		return nil, []string{"inbound links unknown: this server has no graph store"}
	}
	scope, err := t.hooks.Graph(ctx)
	if err != nil {
		return nil, []string{"inbound links unknown: " + err.Error()}
	}
	scope.seed(ctx, target)
	seen := map[string]bool{}
	var sources []string
	for _, bl := range scope.Store.BacklinksEnriched(target.NodeURL) {
		if !seen[bl.URL] {
			seen[bl.URL] = true
			sources = append(sources, bl.URL)
		}
	}
	if len(sources) == 0 {
		return nil, []string{"no inbound links in the graph store; run mark_graph first if the store is cold"}
	}
	if len(sources) > curateSources {
		notes = append(notes, fmt.Sprintf("inbound links read from the first %d of %d linking documents", curateSources, len(sources)))
		sources = sources[:curateSources]
	}
	for _, source := range sources {
		src, err := t.hooks.Resolve(ctx, source)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s not read: %v", source, err))
			continue
		}
		r, err := t.fetch(ctx, src, src.Path)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s not read: %v", source, err))
		case r.Response.Status != protocol.StatusOK:
			notes = append(notes, fmt.Sprintf("%s not read: %s", source, r.Response.Status))
		default:
			inbound = append(inbound, curate.InboundFrom(&curate.Source{Name: source, Path: src.Path, Body: r.Response.Body}, target.Path, target.NodeURL)...)
		}
	}
	return inbound, notes
}
