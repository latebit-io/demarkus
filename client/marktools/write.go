package marktools

import (
	"context"
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
	"github.com/latebit-io/demarkus/client/memory/metaguard"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
)

// WriteFunc runs a WriteOp under the surface's authorization; see docwrite.
type WriteFunc = docwrite.WriteFunc

// WriteOp is one write, sent with the ctx and token it is given.
type WriteOp = docwrite.WriteOp

// writer asks the surface to authorize verb on target; its refusal is the
// tool's answer, word for word.
func (t *Tools) writer(ctx context.Context, target Target, verb string) (WriteFunc, *Result) {
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

// doc binds the shared write contract to one target.
func (t *Tools) doc(ctx context.Context, target Target, write WriteFunc) *docwrite.Doc {
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
	mode, err := merge.ParseOnConflict(args.OnConflict)
	if err != nil {
		return failure("%v", err)
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		return *bad
	}
	w := docwrite.Write{Body: args.Body, ExpectedVersion: expected, Metadata: t.publisherMeta(ctx, args.Metadata)}
	result, notes, bad := t.landPublish(ctx, &landing{target: target, doc: t.doc(ctx, target, write), mode: mode}, &w)
	if bad != nil {
		return *bad
	}
	// A candidate ends with its body; a note after it would read as body.
	return text(formatWrite(&result, writeFields...) + notes)
}

// landing is where a publish lands: the target, its bound document, and
// the conflict mode.
type landing struct {
	target Target
	doc    *docwrite.Doc
	mode   string
}

// landPublish is the write every publish tool makes after its own checks:
// style gate, write, graph, hub link of a first version; notes is what a
// landed write carries after its fields (hubs edited, then the notes).
func (t *Tools) landPublish(ctx context.Context, at *landing, w *docwrite.Write) (result docwrite.Result, notes string, bad *Result) {
	styleNote, bad := t.styleGate(ctx, at.target, w.Body, w.Metadata["description"])
	if bad != nil {
		return docwrite.Result{}, "", bad
	}
	result, err := at.doc.Publish(ctx, *w, at.mode)
	if err != nil {
		failed := t.failed(SitePublish, at.target.Host, err)
		return docwrite.Result{}, "", &failed
	}
	if !landed(&result) {
		return result, "", nil
	}
	t.observePublished(ctx, at.target, w, &result)
	return result, t.linkOnPublish(ctx, at, w, &result) + t.narrowingNote(ctx, at.doc, *w) + styleNote, nil
}

// observePublished merges a landed publish into the graph: the body and
// publisher metadata are in hand, the server's keys come from the response.
func (t *Tools) observePublished(ctx context.Context, target Target, w *docwrite.Write, result *docwrite.Result) {
	meta := maps.Clone(w.Metadata)
	maps.Copy(meta, result.Response.Metadata)
	t.observeWrite(ctx, target, graph.FetchResult{Status: protocol.StatusOK, Body: w.Body, Metadata: meta})
}

// landed is a write the server applied, not a merge candidate to review.
func landed(r *docwrite.Result) bool {
	return r.Candidate == nil && protocol.IsWriteSuccess(r.Response.Status)
}

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

// observeWrite merges a landed write into the graph scope the way explore
// merges a read, so the document and its edges are current without a crawl.
// Best effort: the write already landed; a failed save is only logged.
func (t *Tools) observeWrite(ctx context.Context, target Target, result graph.FetchResult) {
	scope := t.writeScope(ctx)
	if scope == nil {
		return
	}
	// A write's response rides on its metadata; the gate's note is response
	// only and would change the document's hash in the observation.
	delete(result.Metadata, "warning")
	if !scope.observe(target, result) {
		return
	}
	if err := scope.Store.Save(); err != nil {
		t.warnf("warning: graph cache save after write mark://%s%s: %v", target.Host, target.Path, err)
	}
}

// observe merges one landed write without saving; true when it changed the store.
func (s *GraphScope) observe(target Target, result graph.FetchResult) bool {
	if s.Source != nil {
		result.Source = s.Source(target)
	}
	return s.Store.ObserveDocument(target.NodeURL, result)
}

// observed is one landed write for observeWrites.
type observed struct {
	target Target
	result graph.FetchResult
}

// observeWrites is observeWrite for a batch: a save rewrites the whole
// cache, so it runs once after every observation.
func (t *Tools) observeWrites(ctx context.Context, writes []observed) {
	scope := t.writeScope(ctx)
	if scope == nil {
		return
	}
	changed := false
	for i := range writes {
		changed = scope.observe(writes[i].target, writes[i].result) || changed
	}
	if !changed {
		return
	}
	if err := scope.Store.Save(); err != nil {
		t.warnf("warning: graph cache save after %d writes: %v", len(writes), err)
	}
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
	b.WriteString(c.Body)
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

// narrowingNote warns, after a write that landed, when it dropped tags or keys
// the replaced version had. A failed check is logged and never fails the write.
func (t *Tools) narrowingNote(ctx context.Context, doc *docwrite.Doc, w docwrite.Write) string {
	warning, err := metaguard.Gate(ctx, w.ExpectedVersion, w.Metadata, func(ctx context.Context) (fetch.Result, error) {
		return doc.FetchVersion(ctx, w.ExpectedVersion)
	})
	if err != nil {
		t.warnf("warning: publish metadata check mark://%s%s: %v", doc.Host, doc.Path, err)
	}
	if warning == "" {
		return ""
	}
	return mcpfmt.Note(warning)
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
// archives every live document under it.
type ArchiveArgs struct {
	URL       string
	Recursive bool
}

// Bounds on a recursive archive, so one call cannot sweep a whole store.
const (
	archiveListCalls = 100
	archiveDocs      = 500
)

// Archive answers mark_archive. After the archive, every ancestor hub drops
// the bullets that only linked what was archived; a hub that could not be
// edited is a note, the archive stands.
func (t *Tools) Archive(ctx context.Context, args ArchiveArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	if args.Recursive {
		switch {
		case !strings.HasSuffix(target.Path, "/"):
			return failure("recursive needs a directory url ending in /")
		case target.Path == "/":
			return failure("recursive archive of the store root is refused; name a directory under it")
		}
	} else if isContextPath(target.Path) {
		return failure("%s is reserved: the primer is read, never written", ContextLeaf)
	}
	write, bad := t.writer(ctx, target, "archive")
	if bad != nil {
		return *bad
	}
	if !args.Recursive {
		result, err := t.archiveOne(ctx, target, write)
		if err != nil {
			return t.writeFailed(SiteArchive, target.Host, err)
		}
		out := formatResult(&result, "version")
		if landed(&result) {
			out += t.dropHubLinks(ctx, target, write, &unlink{hubs: hub.Ancestors(target.Path), targets: []string{target.Path}})
		}
		return text(out)
	}
	paths, err := t.liveUnder(ctx, target)
	if err != nil {
		return failure("archive failed: %v", err)
	}
	if len(paths) == 0 {
		return failure("archive failed: no live documents under %s", target.Path)
	}
	var b strings.Builder
	var archived []string
	for _, p := range paths {
		result, err := t.archiveOne(ctx, target.at(p), write)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "- %s: failed: %s\n", p, t.writeFailed(SiteArchive, target.Host, err).Text)
		case !landed(&result):
			fmt.Fprintf(&b, "- %s: failed: %s\n", p, result.Response.Status)
		default:
			archived = append(archived, p)
			fmt.Fprintf(&b, "- %s: v%s\n", p, result.Response.Metadata["version"])
		}
	}
	// A bullet linking the directory goes only when nothing live is left
	// under it; a partial sweep unlinks each archived path from its own hubs.
	status, u := "partial", &unlink{hubs: hub.AncestorsOf(archived...), targets: archived}
	if len(archived) == len(paths) {
		status, u = "archived", &unlink{hubs: hub.Ancestors(target.Path), targets: []string{target.Path}}
	}
	dropped := ""
	if len(archived) > 0 {
		dropped = t.dropHubLinks(ctx, target, write, u)
	}
	return text(fmt.Sprintf("status: %s\narchived: %d of %d\n%s%s", status, len(archived), len(paths), b.String(), dropped))
}

// archiveOne archives one document and observes it into the graph.
func (t *Tools) archiveOne(ctx context.Context, target Target, write WriteFunc) (docwrite.Result, error) {
	result, err := t.doc(ctx, target, write).Archive(ctx)
	if err == nil && landed(&result) {
		// A confirmed archive is a complete observation: stale outgoing rows go.
		t.observeWrite(ctx, target, graph.FetchResult{Status: protocol.StatusArchived, Metadata: result.Response.Metadata})
	}
	return result, err
}

// liveUnder lists every live document under the directory, bounded.
func (t *Tools) liveUnder(ctx context.Context, target Target) ([]string, error) {
	var paths []string
	w := listwalk.Walker{Client: t.backend, Host: target.Host, Token: t.readToken(ctx, target.Host), MaxLists: archiveListCalls}
	err := w.Walk(ctx, target.Path, func(docPath string) error {
		if len(paths) >= archiveDocs {
			return fmt.Errorf("over %d documents under %s; archive a narrower directory", archiveDocs, target.Path)
		}
		paths = append(paths, docPath)
		return nil
	})
	return paths, err
}

// unlink names the hubs to edit and what they must stop linking.
type unlink struct{ hubs, targets []string }

// dropHubLinks drops the targets' bullets from the hubs, reported as lines;
// a hub that could not be edited is a note.
func (t *Tools) dropHubLinks(ctx context.Context, target Target, write WriteFunc, u *unlink) string {
	docs := func(p string) *docwrite.Doc { return t.doc(ctx, target.at(p), write) }
	var hubs, notes strings.Builder
	for _, d := range hub.DropFromHubs(ctx, docs, u.hubs, &hub.Drop{Targets: u.targets, Stamp: t.identityMeta(ctx)}) {
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
