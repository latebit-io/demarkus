package marktools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/client/promote"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Dedup bounds: a lookup page that fills may hide a candidate beyond it,
// and a candidate's body is shown up to candidateCap.
const (
	dedupLimit   = 50
	candidateCap = 4 * 1024
)

// PromoteArgs are mark_promote's arguments; which ones a phase reads is in
// the tool description. Metadata is the raw JSON object.
type PromoteArgs struct {
	URL, Phase string
	Body       string
	Metadata   map[string]any
	Source     string
	Target     string
	Via        string
	Stub       bool
	Packet     string
}

// Promote answers mark_promote: one phase of a promotion. The surface says
// which phases it serves; the body serves them all.
func (t *Tools) Promote(ctx context.Context, args PromoteArgs) Result { //nolint:gocritic // arguments by value, like every tool
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	switch args.Phase {
	case promote.PhaseStage:
		return t.promoteStage(ctx, target)
	case promote.PhaseStamp:
		return t.promoteStamp(ctx, target, &args)
	case promote.PhaseRefresh:
		return t.promoteRefresh(ctx, target, &args)
	case promote.PhaseDryRun, promote.PhaseApply:
		return t.promoteDraft(ctx, target, &args)
	}
	return failure("unknown phase %q (%s)", args.Phase, strings.Join(promote.AllPhases, ", "))
}

// promoteHead reads the source a soul phase works on.
func (t *Tools) promoteHead(ctx context.Context, doc *docwrite.Doc) (*docwrite.Head, *Result) {
	head, err := doc.Head(ctx)
	if err != nil {
		bad := t.failed(SiteFetch, doc.Host, err)
		return nil, &bad
	}
	if head.Status != protocol.StatusOK {
		bad := failure("promote failed: fetch %s: %s", doc.Path, head.Status)
		return nil, &bad
	}
	return head, nil
}

func promoteInput(path string, head *docwrite.Head) *promote.Input {
	return &promote.Input{Path: path, Body: head.Body, Version: head.Version, Metadata: head.Metadata}
}

// promoteStage reads the source, its markers and the layout's verdict.
func (t *Tools) promoteStage(ctx context.Context, target Target) Result {
	head, bad := t.promoteHead(ctx, t.doc(ctx, target, nil))
	if bad != nil {
		return *bad
	}
	l, err := t.layoutFor(ctx, target)
	staged := promote.Stage(promoteInput(target.Path, head), &l)
	if err != nil {
		staged.Notes = append(staged.Notes, fmt.Sprintf("layout unread (%v); the default rules decided the candidate line", err))
	}
	return text(staged.Report())
}

// promoteStamp writes the markers back on the source after the knowledge
// publish; the stage packet binds a stub to the body that was staged.
func (t *Tools) promoteStamp(ctx context.Context, target Target, args *PromoteArgs) Result {
	markerTarget, version, err := promote.ParseTarget(args.Target)
	if err != nil {
		return failure("stamp needs the knowledge copy as target: %v", err)
	}
	if strings.TrimSpace(args.Via) == "" {
		return failure("stamp needs via: the knowledge server's name")
	}
	if args.Packet == "" {
		return failure("stamp needs the packet hash from stage")
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		return *bad
	}
	stamp := promote.Stamp{Markers: promote.Markers{Via: strings.TrimSpace(args.Via), Target: markerTarget, Version: version}, Stub: args.Stub, Packet: args.Packet}
	w, err := promote.BackStamp(ctx, t.doc(ctx, target, write), t.identityMeta(ctx), &stamp)
	if err != nil {
		return t.promoteRefused("stamp", target, err)
	}
	t.observeWritten(ctx, target, &w)
	return text(writtenReport("stamped", &w))
}

// promoteRefresh compares the source with the live copy the agent fetched;
// with the packet it writes the planned body.
func (t *Tools) promoteRefresh(ctx context.Context, target Target, args *PromoteArgs) Result {
	if args.Body == "" {
		return failure("refresh needs body: the live knowledge copy")
	}
	liveTarget, liveVersion, err := promote.ParseTarget(args.Target)
	if err != nil {
		return failure("refresh needs the live copy as target: %v", err)
	}
	var write WriteFunc
	if args.Packet != "" {
		var bad *Result
		if write, bad = t.writer(ctx, target, "publish"); bad != nil {
			return *bad
		}
	}
	doc := t.doc(ctx, target, write)
	head, bad := t.promoteHead(ctx, doc)
	if bad != nil {
		return *bad
	}
	plan, err := promote.PlanRefresh(promoteInput(target.Path, head), &promote.Refresh{Live: args.Body, LiveVersion: liveVersion, Target: liveTarget})
	if err != nil {
		return failure("refresh refused: %v", err)
	}
	if args.Packet == "" {
		return text(plan.Report())
	}
	if plan.Packet != args.Packet {
		return failure("packet %s does not match the document at v%d; rerun refresh without the packet and review", args.Packet, head.Version)
	}
	w, err := promote.ApplyRefresh(ctx, doc, t.identityMeta(ctx), &plan)
	if err != nil {
		return t.promoteRefused("refresh", target, err)
	}
	t.observeWritten(ctx, target, &w)
	return text(writtenReport("refreshed", &w))
}

// writtenReport words a soul write that landed.
func writtenReport(status string, w *promote.Written) string {
	out := fmt.Sprintf("status: %s\npath: %s\nversion: %d\nmode: %s\n", status, w.Path, w.Version, w.Mode)
	if w.Note != "" {
		out += "note: " + w.Note + "\n"
	}
	return out
}

// promoteRefused words a soul write that did not land: the document's state
// in the mechanic's words, a transport failure in the surface's.
func (t *Tools) promoteRefused(phase string, target Target, err error) Result {
	var refusal *promote.Refusal
	if errors.As(err, &refusal) {
		return failure("%s refused: %v", phase, err)
	}
	return t.writeFailed(SitePublish, target.Host, err)
}

// observeWritten records a landed soul write in the graph.
func (t *Tools) observeWritten(ctx context.Context, target Target, w *promote.Written) {
	t.observeWrite(ctx, target, graph.FetchResult{Status: protocol.StatusOK, Body: w.Body, Metadata: w.Metadata})
}

// promoteDraft is the knowledge side: dry_run checks and dedups the draft,
// apply rechecks it against the packet and publishes with provenance.
func (t *Tools) promoteDraft(ctx context.Context, target Target, args *PromoteArgs) Result {
	apply := args.Phase == promote.PhaseApply
	if args.Body == "" {
		return failure("%s needs body: the distilled draft", args.Phase)
	}
	if apply && args.Packet == "" {
		return failure("apply needs the packet hash from dry_run")
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		bad.Text = "status: not-permitted\n" + bad.Text
		return *bad
	}
	draft := &promote.Draft{Path: target.Path, Body: args.Body, Metadata: callerMeta(args.Metadata), Source: strings.TrimSpace(args.Source)}
	doc := t.doc(ctx, target, write)
	head, err := doc.Head(ctx)
	if err != nil {
		return t.failed(SiteFetch, target.Host, err)
	}
	switch head.Status {
	case protocol.StatusOK:
		draft.Version = head.Version
	case protocol.StatusNotFound:
	default:
		return failure("%s refused: the target is %s", args.Phase, head.Status)
	}
	policy, bad := t.worldPolicy(ctx, target)
	if bad != nil {
		return *bad
	}
	check := promote.Examine(draft, policy)
	if !apply {
		return t.promoteDryRun(ctx, target, check, head)
	}
	if check.Packet != args.Packet {
		return failure("packet %s does not match the draft against the target at v%d; rerun dry_run and review", args.Packet, draft.Version)
	}
	return t.promoteApply(ctx, target, doc, draft)
}

// worldPolicy reads the world's write policy; not-found is no policy, any
// other failure refuses, since the policy is what the gate checks against.
func (t *Tools) worldPolicy(ctx context.Context, target Target) (*publishpolicy.Policy, *Result) {
	r, err := t.fetch(ctx, target, publishpolicy.DocumentPath)
	switch {
	case err != nil:
		bad := failure("status: policy-unavailable\n%s", t.errText(SiteFetch, target.Host, err))
		return nil, &bad
	case r.Response.Status == protocol.StatusNotFound:
		return nil, nil
	case r.Response.Status != protocol.StatusOK:
		bad := failure("status: policy-unavailable\nfetch %s: %s", publishpolicy.DocumentPath, r.Response.Status)
		return nil, &bad
	}
	policy := publishpolicy.Parse(r.Response.Body)
	return &policy, nil
}

// promoteDryRun fills the dedup findings and renders the report.
func (t *Tools) promoteDryRun(ctx context.Context, target Target, check *promote.Check, head *docwrite.Head) Result {
	if bad := t.dedup(ctx, target, check); bad != nil {
		return *bad
	}
	truncated := "[truncated]"
	for i := range check.Candidates {
		c := &check.Candidates[i]
		if !c.Fetch {
			continue
		}
		if c.Path == target.Path {
			c.Version, c.Body = head.Version, clip(head.Body, candidateCap, truncated)
			continue
		}
		d, err := FetchBody(t.fetch(ctx, target, c.Path))
		if err != nil {
			check.Notes = append(check.Notes, fmt.Sprintf("%s not read: %v", c.Path, err))
			continue
		}
		c.Version, c.Body = d.Version, clip(d.Body, candidateCap, truncated)
	}
	return text("target: " + target.name() + "\n" + check.Report())
}

// dedup runs the skill's two lookups: the title in body mode, then the
// subject tags in the catalog when the title found nothing. A lookup that
// cannot be trusted ends the dry run as incomplete-dedup.
func (t *Tools) dedup(ctx context.Context, target Target, check *promote.Check) *Result {
	title := check.Draft.Title()
	if title == "" {
		check.Notes = append(check.Notes, "no H1: dedup by title skipped")
	} else {
		rows, catalogOnly, err := t.lookupRows(ctx, target, title, fetch.MatchBody)
		if err != nil {
			bad := failure("status: incomplete-dedup\n%v", err)
			return &bad
		}
		if catalogOnly {
			check.Notes = append(check.Notes, "catalog-only: "+fetch.CatalogFallbackNote)
		}
		check.Candidates, check.Related = promote.Candidates(rows, &check.Draft, false)
	}
	if len(check.Candidates) > 0 {
		return nil
	}
	tags := check.Draft.SubjectTags()
	if len(tags) == 0 {
		return nil
	}
	rows, _, err := t.lookupRows(ctx, target, strings.Join(tags[:min(3, len(tags))], " "), "")
	if err != nil {
		bad := failure("status: incomplete-dedup\n%v", err)
		return &bad
	}
	var related []string
	check.Candidates, related = promote.Candidates(rows, &check.Draft, true)
	for _, r := range related {
		if !slices.Contains(check.Related, r) {
			check.Related = append(check.Related, r)
		}
	}
	return nil
}

// lookupRows is one lookup over the world; a page that fills is an error,
// since a candidate may be beyond it.
func (t *Tools) lookupRows(ctx context.Context, target Target, query, match string) (rows []lookuptable.Row, catalogOnly bool, err error) {
	req := fetch.LookupRequest{Host: target.Host, Scope: "/", Token: t.readToken(ctx, target.Host), Query: query, Limit: dedupLimit, Match: match}
	r, err := t.backend.Lookup(ctx, req)
	if err != nil {
		return nil, false, errors.New(t.errText(SiteLookup, target.Host, err))
	}
	if r.Response.Status != protocol.StatusOK {
		return nil, false, fmt.Errorf("lookup %q: %s", query, r.Response.Status)
	}
	table, err := lookuptable.ParseTable(r.Response.Body)
	if err != nil {
		return nil, false, err
	}
	if len(table.Rows) >= dedupLimit {
		return nil, false, fmt.Errorf("lookup %q: %d or more matches; narrow the draft's title or tags", query, dedupLimit)
	}
	return table.Rows, fetch.AnsweredFromCatalog(req, r), nil
}

// promoteApply publishes the checked draft as mark_publish would, with the
// provenance key, at the version the dry run saw; the server's policy
// decides the write as it does for every publish.
func (t *Tools) promoteApply(ctx context.Context, target Target, doc *docwrite.Doc, draft *promote.Draft) Result {
	meta := maps.Clone(draft.Metadata)
	maps.Copy(meta, t.identityMeta(ctx))
	if draft.Source != "" {
		meta[promote.PromotedFrom] = draft.Source
	}
	w := docwrite.Write{Body: draft.Body, ExpectedVersion: draft.Version, Metadata: meta}
	result, notes, bad := t.landPublish(ctx, &landing{target: target, doc: doc, mode: merge.OnConflictFail}, &w)
	if bad != nil {
		return *bad
	}
	if result.Response.Status == protocol.StatusConflict {
		return failure("apply refused: conflict: the target moved since the dry run; rerun dry_run")
	}
	if !landed(&result) {
		return failure("apply refused: publish %s: %s", target.Path, result.Response.Status)
	}
	version, err := docwrite.Version(result.Response.Metadata)
	if err != nil {
		return failure("apply: %v", err)
	}
	return text(fmt.Sprintf("status: published\ntarget: %s\nversion: %d\n", target.name(), version) + notes)
}

// callerMeta is the caller's metadata as strings; identity keys are the
// surface's, never the caller's.
func callerMeta(raw map[string]any) map[string]string {
	meta := make(map[string]string, len(raw))
	for k, v := range raw {
		if !protocol.IsIdentityMetadataKey(k) {
			meta[k] = fmt.Sprintf("%v", v)
		}
	}
	return meta
}
