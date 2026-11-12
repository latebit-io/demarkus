package marktools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/client/memory/journal"
	"github.com/latebit-io/demarkus/client/memory/landing"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/docframe"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// WriteFunc runs a WriteOp under the surface's authorization; see docwrite.
type WriteFunc = docwrite.WriteFunc

// WriteOp is one write, sent with the ctx and token it is given.
type WriteOp = docwrite.WriteOp

// errGraphData refuses a write to generated graph data: it is derived, and a
// hand write breaks what seeds from it.
func errGraphData(p string) error {
	return fmt.Errorf("%s is generated graph data: derived from the documents, never written directly", p)
}

// writer is authorize for a document tool, which never writes generated
// graph data: refused first, before any request.
func (t *Tools) writer(ctx context.Context, target Target, verb string) (WriteFunc, *Result) {
	if storefmt.GeneratedGraphTarget(target.Path) {
		bad := failure("%v", errGraphData(target.Path))
		return nil, &bad
	}
	return t.authorize(ctx, target, verb)
}

// authorize asks the surface to authorize verb on target; its refusal is the
// tool's answer, word for word.
func (t *Tools) authorize(ctx context.Context, target Target, verb string) (WriteFunc, *Result) {
	if t.hooks.Writer == nil {
		bad := failure("%s is not available: this server has no writer", verb)
		return nil, &bad
	}
	write, err := t.hooks.Writer(ctx, target, verb)
	if err != nil {
		bad := failure("%v", err)
		return nil, &bad
	}
	return write, nil
}

// doc is bind for a document tool. writer saw the argument; a path the tool
// derived from it (a journal, a split topic) is refused here, at the write.
func (t *Tools) doc(ctx context.Context, target Target, write WriteFunc) *docwrite.Doc {
	if write != nil && storefmt.GeneratedGraphTarget(target.Path) {
		write = func(context.Context, WriteOp) (fetch.Result, error) { return fetch.Result{}, errGraphData(target.Path) }
	}
	return t.bind(ctx, target, write)
}

// bind binds the shared write contract to one target.
func (t *Tools) bind(ctx context.Context, target Target, write WriteFunc) *docwrite.Doc {
	return &docwrite.Doc{
		Backend: t.backend, Host: target.Host, Path: target.Path,
		ReadToken: t.readToken(ctx, target.Host), Write: write,
	}
}

// writeFields are the response keys a write tool shows; warning is what
// the server's gate let through.
var writeFields = []string{"version", "modified", "server-version", "warning"}

// PublishArgs are mark_publish's arguments. ExpectedVersion is nil when the
// caller left it out; Metadata is the raw JSON object.
type PublishArgs struct {
	URL, Body       string
	ExpectedVersion *int
	OnConflict      string
	Metadata        map[string]string
}

// Publish answers mark_publish. On a conflict the default mode returns a merge
// candidate for the agent to review; it never publishes the merge itself.
func (t *Tools) Publish(ctx context.Context, args PublishArgs) Result { //nolint:gocritic // arguments by value, like every tool
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	// Arguments, then authorization, then the network: on every write tool.
	if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	expected, bad := requireVersion(args.ExpectedVersion)
	if bad != nil {
		return *bad
	}
	mode, err := merge.ModeOf("on_conflict", args.OnConflict)
	if err != nil {
		return failure("%v", err)
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		return *bad
	}
	w := docwrite.Write{Body: args.Body, ExpectedVersion: expected, Metadata: t.publisherMeta(ctx, args.Metadata)}
	result, notes, bad := t.landPublish(ctx, &publishAt{target: target, doc: t.doc(ctx, target, write), mode: mode}, &w)
	if bad != nil {
		return *bad
	}
	// A candidate ends with its body; a note after it would read as body.
	return text(formatWrite(&result, writeFields...) + notes)
}

// publishAt is where a publish lands: the target, its bound document, and
// the conflict mode.
type publishAt struct {
	target Target
	doc    *docwrite.Doc
	mode   merge.OnConflict
}

// landPublish is the write every publish tool makes after its own checks
// (landing.Publish), observed into the graph; notes is what a landed write
// carries after its fields (hubs edited, then the notes).
func (t *Tools) landPublish(ctx context.Context, at *publishAt, w *docwrite.Write) (result docwrite.Result, notes string, bad *Result) {
	out, err := landing.Publish(ctx, landing.Beside(at.doc), &landing.PublishRequest{
		Path: at.target.Path, Write: *w, Mode: at.mode, Style: t.stylePolicy(ctx, at.target), Stamp: t.identityMeta(ctx),
	})
	if refused, ok := errors.AsType[*landing.StyleRefusal](err); ok {
		blocked := failure("publish refused, style problems: %s Fix the body and republish.", refused.Findings)
		return docwrite.Result{}, "", &blocked
	}
	if err != nil {
		failed := t.failed(SitePublish, at.target.Host, err)
		return docwrite.Result{}, "", &failed
	}
	if !out.Landed {
		return out.Result, "", nil
	}
	t.observePublished(ctx, at.target, w, &out.Result)
	if out.NarrowedErr != nil {
		t.warnf("warning: publish metadata check mark://%s%s: %v", at.doc.Host, at.doc.Path, out.NarrowedErr)
	}
	return out.Result, publishNotes(&out), nil
}

// publishNotes words what landed with a publish: the hub edited or why not,
// what the update dropped, the style findings.
func publishNotes(out *landing.Published) string {
	notes := ""
	switch {
	case out.Hub.Added:
		notes = "hubs:\n- " + out.Hub.Hub + ": bullet added\n"
	case out.Hub.Problem != "":
		notes = mcpfmt.Note(out.Hub.Problem)
	}
	if out.Narrowed != "" {
		notes += mcpfmt.Note(out.Narrowed)
	}
	if out.StyleFindings != "" {
		notes += mcpfmt.Note("style problems in this publish: " + out.StyleFindings)
	}
	return notes
}

// observePublished merges a landed publish into the graph: the body and
// publisher metadata are in hand, the server's keys come from the response.
func (t *Tools) observePublished(ctx context.Context, target Target, w *docwrite.Write, result *docwrite.Result) {
	meta := maps.Clone(w.Metadata)
	maps.Copy(meta, result.Response.Metadata)
	t.observeWrite(ctx, target, graph.FetchResult{Status: protocol.StatusOK, Body: w.Body, Metadata: meta})
}

// landed is a write the server applied, not a merge candidate to review.
func landed(r *docwrite.Result) bool { return landing.Landed(r) }

// writeScope is the graph scope a landed write is observed into; nil when the
// surface has none for this call, which is not a failure of the write.
func (t *Tools) writeScope(ctx context.Context) *GraphScope {
	if t.hooks.Graph == nil {
		return nil
	}
	scope, err := t.hooks.Graph(ctx)
	if err != nil {
		return nil // the scope's refusal is explore's answer, not a write's
	}
	return scope
}

// observed is one landed write on its way into the graph.
type observed struct {
	target Target
	result graph.FetchResult
}

// observeWrite merges a landed write into the graph scope the way explore
// merges a read, so the document and its edges are current without a crawl.
func (t *Tools) observeWrite(ctx context.Context, target Target, result graph.FetchResult) {
	t.observeWrites(ctx, []observed{{target, result}})
}

// observeWrites merges a batch of landed writes and saves once: a save
// rewrites the whole cache. Best effort: the writes landed; a failed save
// is only logged.
func (t *Tools) observeWrites(ctx context.Context, writes []observed) {
	scope := t.writeScope(ctx)
	if scope == nil || len(writes) == 0 {
		return
	}
	kept := writes[:0:0]
	for i := range writes {
		w := writes[i]
		// A write's response rides on its metadata; the gate's note is response
		// only and would change the document's hash in the observation.
		delete(w.result.Metadata, "warning")
		// An older server answers a write without its etag; observed, the write
		// would read as a conflict against the fetch after it, so that fetch
		// observes it instead.
		if w.result.Status == protocol.StatusOK && w.result.Metadata["etag"] == "" {
			continue
		}
		kept = append(kept, w)
	}
	if err := scope.record(kept); err != nil {
		what := fmt.Sprintf("%d writes", len(writes))
		if len(writes) == 1 {
			what = "write mark://" + writes[0].target.Host + writes[0].target.Path
		}
		t.warnf("warning: graph cache save after %s: %v", what, err)
	}
}

// record merges observations into the store and saves once when any changed
// it; reads and writes share it, so both record under the scope's Source.
func (s *GraphScope) record(seen []observed) error {
	changed := false
	for i := range seen {
		o := &seen[i]
		if s.Source != nil {
			o.result.Source = s.Source(o.target)
		}
		changed = s.Store.ObserveDocument(o.target.NodeURL, o.result) || changed
	}
	if !changed {
		return nil
	}
	return s.Store.Save()
}

// observeAppended reads the appended head once; the client never holds the
// full body of an append. Skipped without a scope so no fetch is wasted.
func (t *Tools) observeAppended(ctx context.Context, target Target, doc *docwrite.Doc, result *docwrite.Result) {
	if t.writeScope(ctx) == nil {
		return
	}
	version, err := strconv.Atoi(result.Response.Metadata["version"])
	if err != nil || version < 1 {
		t.warnf("warning: graph observation after append mark://%s%s: response carries no version", target.Host, target.Path)
		return
	}
	head, err := doc.FetchVersion(ctx, version)
	if err != nil || head.Response.Status != protocol.StatusOK {
		t.warnf("warning: graph observation after append mark://%s%s: read of v%d failed: status %q err %v", target.Host, target.Path, version, head.Response.Status, err)
		return
	}
	t.observeWrite(ctx, target, graph.FetchResult{Status: protocol.StatusOK, Body: head.Response.Body, Metadata: head.Response.Metadata})
}

// publisherMeta is the caller's metadata under the surface's identity: a
// caller cannot write under another agent's name.
func (t *Tools) publisherMeta(ctx context.Context, raw map[string]string) map[string]string {
	meta := callerMeta(raw)
	maps.Copy(meta, t.identityMeta(ctx))
	return meta
}

// requireVersion is the check every version checked publish makes.
func requireVersion(given *int) (int, *Result) {
	if given == nil {
		bad := failure("expected_version is required")
		return 0, &bad
	}
	if *given < 0 {
		bad := failure("expected_version must be >= 0")
		return 0, &bad
	}
	return *given, nil
}

// identityMeta is the surface's stamp on every write: the identity keys its
// hooks supply.
func (t *Tools) identityMeta(ctx context.Context) map[string]string {
	meta := map[string]string{}
	if t.hooks.Agent != nil {
		meta[protocol.MetaAgent] = t.hooks.Agent(ctx)
	}
	if t.hooks.User != nil {
		meta[protocol.MetaUser] = t.hooks.User(ctx)
	}
	return meta
}

// formatResult renders a write; a reconciled one reads like an answered one. A
// candidate carries the merge facts, then the body to review.
func formatResult(r *docwrite.Result, fields ...string) string {
	c := r.Candidate
	if c == nil {
		return mcpfmt.Full(fetch.Result{Response: r.Response}, fields...)
	}
	var b strings.Builder
	b.WriteString("status: " + docwrite.StatusCandidate + "\n")
	fmt.Fprintf(&b, "your-version: %d\n", c.BaseVersion)
	fmt.Fprintf(&b, "current-version: %d\n", c.TheirVersion)
	fmt.Fprintf(&b, "publish-at-version: %d\n", c.PublishAtVersion)
	fmt.Fprintf(&b, "has-markers: %t\n", c.HasMarkers)
	b.WriteString("\n")
	b.WriteString(docframe.Frame(c.Body) + docframe.Flags(c.Body, nil))
	return b.String()
}

// formatWrite is formatResult with the version delta on the status line:
// "created v9 (was v8)" reads as the update it is, "created v1" as new.
func formatWrite(r *docwrite.Result, fields ...string) string {
	v, err := docwrite.Version(r.Response.Metadata)
	if err != nil || v < 1 || r.Response.Status != protocol.StatusCreated {
		return formatResult(r, fields...)
	}
	status := fmt.Sprintf("created v%d", v)
	if v > 1 {
		status += fmt.Sprintf(" (was v%d)", v-1)
	}
	return mcpfmt.FullStatus(status, r.Response.Metadata, fields...)
}

// AppendArgs are mark_append's arguments; ExpectedVersion 0 resolves the
// current version. Route today: URL is a directory, the target its dated
// journal (created when absent) and Tags the entry's subjects.
type AppendArgs struct {
	URL, Body       string
	ExpectedVersion int
	Route, Tags     string
}

// RouteToday is the journal route.
const RouteToday = "today"

// Append answers mark_append.
func (t *Tools) Append(ctx context.Context, args AppendArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	if args.ExpectedVersion < 0 {
		return failure("expected_version must be >= 0")
	}
	if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	switch args.Route {
	case "":
	case RouteToday:
		return t.appendToday(ctx, target, args)
	default:
		return failure("invalid route %q: expected \"today\" or none", args.Route)
	}
	write, bad := t.writer(ctx, target, "append")
	if bad != nil {
		return *bad
	}
	doc := t.doc(ctx, target, write)
	result, err := doc.Append(ctx, docwrite.AppendRequest{
		Body: args.Body, ExpectedVersion: args.ExpectedVersion, Metadata: t.identityMeta(ctx),
	})
	if err != nil {
		return t.writeFailed(SiteAppend, target.Host, err)
	}
	if landed(&result) {
		t.observeAppended(ctx, target, doc, &result)
	}
	return text(formatWrite(&result, writeFields...))
}

// appendToday records the entry in today's journal under the target
// directory; the surface's one authorization covers the hubs it keeps current.
func (t *Tools) appendToday(ctx context.Context, target Target, args AppendArgs) Result { //nolint:gocritic // arguments by value, like every tool
	write, bad := t.writer(ctx, target, "append")
	if bad != nil {
		return *bad
	}
	docs := func(p string) *docwrite.Doc { return t.doc(ctx, target.at(p), write) }
	l, err := t.layoutFor(ctx, target)
	if err != nil {
		return failure("append failed: %v", err)
	}
	out, err := journal.Append(ctx, docs, journal.Request{
		Dir: target.Path, Entry: args.Body, Tags: protocol.SplitTags(args.Tags),
		Stamp: t.identityMeta(ctx), Layout: l, Now: t.now(),
	})
	if err != nil {
		return failure("append failed: %v", err)
	}
	if out.Status != journal.StatusPresent {
		landed := docwrite.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": strconv.Itoa(out.Version)}}}
		t.observeAppended(ctx, target.at(out.Path), docs(out.Path), &landed)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\nversion: %d\npath: %s\n", out.Status, out.Version, out.Path)
	for _, note := range out.Notes {
		b.WriteString(mcpfmt.Note(note))
	}
	return text(b.String())
}

// ArchiveArgs are mark_archive's arguments; Recursive takes a directory and
// archives every live document under it. ExpectedDocs, when positive, is the
// live count the caller gated: a different count refuses the sweep.
type ArchiveArgs struct {
	URL          string
	Recursive    bool
	ExpectedDocs int
}

// Archive answers mark_archive (landing.Archive, landing.ArchiveUnder): after
// the archive, the ancestor hubs drop the bullets that only linked what was
// archived; a hub that could not be edited is a note, the archive stands.
func (t *Tools) Archive(ctx context.Context, args ArchiveArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	if args.Recursive {
		// Refused before authorization, as an argument is.
		switch {
		case !strings.HasSuffix(target.Path, "/"):
			return failure("%v", landing.ErrNotADirectory)
		case target.Path == "/":
			return failure("%v", landing.ErrStoreRoot)
		}
	} else if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	write, bad := t.writer(ctx, target, "archive")
	if bad != nil {
		return *bad
	}
	docs := func(p string) *docwrite.Doc { return t.doc(ctx, target.at(p), write) }
	if args.Recursive {
		return t.archiveUnder(ctx, target, docs, args.ExpectedDocs)
	}
	out, err := landing.Archive(ctx, docs, target.Path, t.identityMeta(ctx))
	if err != nil {
		return t.writeFailed(SiteArchive, target.Host, err)
	}
	if out.Landed {
		t.observeWrites(ctx, []observed{archivedAt(target, &out.Result)})
	}
	return text(formatResult(&out.Result, "version") + droppedText(out.Hubs))
}

// archiveUnder answers a recursive archive: one line per document, then the hubs.
func (t *Tools) archiveUnder(ctx context.Context, target Target, docs hub.Docs, expected int) Result {
	out, err := landing.ArchiveUnder(ctx, docs, &landing.Sweep{
		Dir: target.Path, ExpectedDocs: expected, Stamp: t.identityMeta(ctx),
		Walker: listwalk.Walker{Client: t.backend, Host: target.Host, Token: t.readToken(ctx, target.Host), SkipGraphData: true},
	})
	if err != nil {
		return failure("%v", err)
	}
	var b strings.Builder
	for i := range out.Docs {
		d := &out.Docs[i]
		switch {
		case d.Err != nil:
			fmt.Fprintf(&b, "- %s: failed: %s\n", d.Path, t.writeFailed(SiteArchive, target.Host, d.Err).Text)
		case !landed(&d.Result):
			fmt.Fprintf(&b, "- %s: failed: %s\n", d.Path, d.Result.Response.Status)
		default:
			fmt.Fprintf(&b, "- %s: v%s\n", d.Path, d.Result.Response.Metadata["version"])
		}
	}
	// One save for the sweep, not one per document.
	seen := make([]observed, 0, len(out.Archived))
	for i := range out.Archived {
		seen = append(seen, archivedAt(target.at(out.Archived[i].Path), &out.Archived[i].Result))
	}
	t.observeWrites(ctx, seen)
	status := "partial"
	if out.Complete {
		status = "archived"
	}
	return text(fmt.Sprintf("status: %s\narchived: %d of %d\n%s%s", status, len(out.Archived), len(out.Docs), b.String(), droppedText(out.Hubs)))
}

// archivedAt is a confirmed archive as an observation: complete, so the
// document's stale outgoing rows go.
func archivedAt(target Target, result *docwrite.Result) observed {
	return observed{target, graph.FetchResult{Status: protocol.StatusArchived, Metadata: result.Response.Metadata}}
}

// droppedText words the hub bullets an archive dropped, as lines; a hub that
// could not be edited is a note.
func droppedText(dropped []hub.Dropped) string {
	var hubs, notes strings.Builder
	for _, d := range dropped {
		switch {
		case d.Err != nil:
			notes.WriteString(mcpfmt.Note(d.Err.Error()))
		case d.Dropped > 0:
			if hubs.Len() == 0 {
				hubs.WriteString("hubs:\n")
			}
			fmt.Fprintf(&hubs, "- %s: %d bullet(s) dropped\n", d.Hub, d.Dropped)
		}
	}
	return hubs.String() + notes.String()
}
