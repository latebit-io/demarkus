package marktools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/memory/audit"
	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/latebit-io/demarkus/client/memory/worldpolicy"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/docframe"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// candidateCap bounds the body a dry run shows of each dedup candidate.
const candidateCap = 4 * 1024

// PromoteArgs are mark_promote's arguments; which ones a phase reads is in
// the tool description. Metadata is the raw JSON object.
type PromoteArgs struct {
	URL, Phase string
	Body       string
	Metadata   map[string]string
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
		switch {
		case args.Target != "" || args.Packet != "":
		case strings.HasSuffix(target.Path, "/"):
			return t.promoteInventory(ctx, target)
		default:
			return t.promoteRow(ctx, target)
		}
		return failure("refresh needs body: the live knowledge copy")
	}
	// Input, not the write: a stub derives from it, so docwrite never sees it.
	if docframe.HasFrame(args.Body) {
		return failure("refresh refused: %v", docwrite.ErrFramed)
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

// promoteInventory answers refresh on a directory: every promoted document
// under it with the copy it tracks, walked in full, so a refresh starts from
// the store rather than a capped tag lookup.
func (t *Tools) promoteInventory(ctx context.Context, target Target) Result {
	store := &audit.ClientStore{Client: t.backend, Host: target.Host, Token: t.readToken(ctx, target.Host)}
	inv, err := audit.Promoted(ctx, store, target.Path, doctorTimeout)
	if err != nil {
		return failure("refresh inventory failed: %v", err)
	}
	return text(inventoryReport(target, inv))
}

// promoteRow answers refresh on one document with no body: its own inventory
// row from the head it has to read anyway, so a scoped refresh walks nothing.
func (t *Tools) promoteRow(ctx context.Context, target Target) Result {
	head, bad := t.promoteHead(ctx, t.doc(ctx, target, nil))
	if bad != nil {
		return *bad
	}
	inv := &audit.Inventory{Documents: 1}
	if d, ok := audit.Classify(target.Path, head.Metadata, head.Body); ok {
		inv.Docs = append(inv.Docs, d)
	}
	return text(inventoryReport(target, inv))
}

// inventoryReport words an inventory; each row names the document as the
// agent passes it back to refresh it.
func inventoryReport(target Target, inv *audit.Inventory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: inventory\npath: %s\ndocuments: %d\npromoted: %d\n", target.Path, inv.Documents, len(inv.Docs))
	for _, gap := range inv.Coverage {
		fmt.Fprintf(&b, "coverage: %s\n", gap)
	}
	for _, d := range inv.Docs {
		name, m := target.at(d.Path).name(), d.Markers
		switch {
		case m.Target == "":
			fmt.Fprintf(&b, "- %s: tag only, no marker\n", name)
		case m.Version == 0:
			fmt.Fprintf(&b, "- %s: promoted-via none (legacy marker); promoted %s\n", name, m.Target)
		default:
			fmt.Fprintf(&b, "- %s: promoted-via %s; promoted %s@v%d\n", name, m.Via, m.Target, m.Version)
		}
	}
	return b.String()
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
	if _, refused := errors.AsType[*promote.Refusal](err); refused {
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
	// A promotion never prunes the copy it lands on: retention is the
	// declaring write's own, confirmed by a human on mark_publish.
	if _, prunes := args.Metadata["retention"]; prunes {
		return failure("%s refused: metadata.retention would prune the target's history; drop it", args.Phase)
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
	policy, bad := t.effectivePolicy(ctx, target)
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

// effectivePolicy is the policy the server will judge a write at target by
// (worldpolicy.Effective): the world's own over the floor world's when the
// surface names one. Nil when neither has a policy document.
func (t *Tools) effectivePolicy(ctx context.Context, target Target) (*publishpolicy.Policy, *Result) {
	own, bad := t.worldPolicy(ctx, target)
	if bad != nil {
		return nil, bad
	}
	floor, bad := t.floorPolicy(ctx, target)
	if bad != nil {
		return nil, bad
	}
	return worldpolicy.Effective(own, floor, target.Path), nil
}

// worldPolicy reads the world's write policy; one that cannot be read
// refuses, since the policy is what the gate checks against.
func (t *Tools) worldPolicy(ctx context.Context, target Target) (*publishpolicy.Policy, *Result) {
	policy, err := worldpolicy.Read(ctx, func(ctx context.Context, p string) (protocol.Response, error) {
		r, err := t.fetch(ctx, target, p)
		return r.Response, err
	})
	unavailable, failed := errors.AsType[*worldpolicy.Unavailable](err)
	switch {
	case failed && unavailable.Err != nil:
		bad := failure("status: policy-unavailable\n%s", t.errText(SiteFetch, target.Host, unavailable.Err))
		return nil, &bad
	case err != nil:
		bad := failure("status: policy-unavailable\n%v", err)
		return nil, &bad
	}
	return policy, nil
}

// promoteDryRun fills the dedup findings (promote.Search) and renders the
// report. A lookup that cannot be trusted ends it as incomplete-dedup.
func (t *Tools) promoteDryRun(ctx context.Context, target Target, check *promote.Check, head *docwrite.Head) Result {
	search := promote.Search{
		CatalogNote: mcpfmt.CatalogFallbackNote,
		Lookup: func(ctx context.Context, req fetch.LookupRequest) (fetch.Result, error) {
			req.Host, req.Token = target.Host, t.readToken(ctx, target.Host)
			r, err := t.backend.Lookup(ctx, req)
			if err != nil {
				return r, errors.New(t.errText(SiteLookup, target.Host, err))
			}
			return r, nil
		},
	}
	if err := search.Dedup(ctx, check); err != nil {
		return failure("status: incomplete-dedup\n%v", err)
	}
	for i := range check.Candidates {
		c := &check.Candidates[i]
		if !c.Fetch {
			continue
		}
		if c.Path == target.Path {
			c.Version, c.Metadata = head.Version, head.Metadata
			c.Body, c.Truncated = bound(head.Body, candidateCap)
			continue
		}
		d, err := FetchBody(t.fetch(ctx, target, c.Path))
		if err != nil {
			check.Notes = append(check.Notes, fmt.Sprintf("%s not read: %v", c.Path, err))
			continue
		}
		c.Version, c.Metadata = d.Version, d.Metadata
		c.Body, c.Truncated = bound(d.Body, candidateCap)
	}
	return text("target: " + target.name() + "\n" + check.Report())
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
	result, notes, bad := t.landPublish(ctx, &publishAt{target: target, doc: doc, mode: merge.OnConflictFail}, &w)
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
	out := fmt.Sprintf("status: published\ntarget: %s\nversion: %d\n", target.name(), version)
	if t.hooks.System != "" {
		out += "via: " + t.hooks.System + "\n"
	}
	return text(out + notes)
}

// callerMeta is the caller's metadata without identity keys (the surface
// stamps agent and user) or a server stamp.
func callerMeta(raw map[string]string) map[string]string {
	meta := make(map[string]string, len(raw))
	for k, v := range raw {
		if !protocol.IsIdentityMetadataKey(k) && !protocol.IsServerStampedKey(k) {
			meta[k] = v
		}
	}
	return meta
}
