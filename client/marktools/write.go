package marktools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/client/metaguard"
	"github.com/latebit-io/demarkus/protocol"
)

// WriteFunc runs a write with a token; see docwrite.WriteFunc.
type WriteFunc = docwrite.WriteFunc

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

// writeFields are the response keys a write tool shows.
var writeFields = []string{"version", "modified", "server-version"}

// PublishArgs are mark_publish's arguments. ExpectedVersion is nil when the
// caller left it out; Metadata is the raw JSON object.
type PublishArgs struct {
	URL, Body       string
	ExpectedVersion *int
	OnConflict      string
	Metadata        map[string]any
}

// Publish answers mark_publish. On a conflict the default mode returns a merge
// candidate for the agent to review; it never publishes the merge itself.
func (t *Tools) Publish(ctx context.Context, args PublishArgs) Result { //nolint:gocritic // arguments by value, like every tool
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	write, bad := t.writer(ctx, target, "publish")
	if bad != nil {
		return *bad
	}
	expected, bad := requireVersion(args.ExpectedVersion)
	if bad != nil {
		return *bad
	}
	mode, err := merge.ParseOnConflict(args.OnConflict)
	if err != nil {
		return failure("%v", err)
	}
	doc := t.doc(ctx, target, write)
	w := merge.Write{Path: target.Path, Body: args.Body, ExpectedVersion: expected, Metadata: t.publisherMeta(ctx, args.Metadata)}
	outcome, err := doc.Publish(ctx, w, mode)
	if err != nil {
		return t.failed(SitePublish, target.Host, err)
	}
	return text(formatOutcome(&outcome) + t.narrowingNote(ctx, doc, w, outcome.Publish.Status))
}

// publisherMeta is the caller's metadata under the surface's identity: a
// caller cannot write under another agent's name.
func (t *Tools) publisherMeta(ctx context.Context, raw map[string]any) map[string]string {
	meta := t.agentMeta(ctx)
	for k, v := range raw {
		if k != "agent" {
			meta[k] = fmt.Sprintf("%v", v)
		}
	}
	return meta
}

// MetadataArg is a tool call's optional "metadata" object. PUBLISH replaces the
// metadata map, so a mistyped argument is refused, never read as none.
func MetadataArg(args map[string]any) (map[string]any, error) {
	raw, given := args["metadata"]
	if !given || raw == nil {
		return nil, nil
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("metadata must be an object of key/value pairs")
	}
	return meta, nil
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

func (t *Tools) agentMeta(ctx context.Context) map[string]string {
	meta := map[string]string{}
	if t.hooks.Agent != nil {
		meta["agent"] = t.hooks.Agent(ctx)
	}
	return meta
}

// formatResult renders a write; a reconciled one reads like an answered one.
func formatResult(r *docwrite.Result, fields ...string) string {
	return mcpfmt.Full(fetch.Result{Response: r.Response}, fields...)
}

// formatOutcome renders a merge outcome. OutcomeOK reads exactly like a plain
// publish; a candidate carries the merge facts, then the body to review.
func formatOutcome(o *merge.Outcome) string {
	if o.Status != merge.OutcomeCandidate {
		return mcpfmt.Full(fetch.Result{Response: protocol.Response{Status: o.Publish.Status, Metadata: o.Publish.Metadata, Body: o.Publish.Body}}, writeFields...)
	}
	var b strings.Builder
	b.WriteString("status: merge-candidate\n")
	fmt.Fprintf(&b, "your-version: %d\n", o.BaseVersion)
	fmt.Fprintf(&b, "current-version: %d\n", o.TheirVersion)
	fmt.Fprintf(&b, "publish-at-version: %d\n", o.PublishAtVersion)
	fmt.Fprintf(&b, "has-markers: %t\n", o.HasMarkers)
	b.WriteString("\n")
	b.WriteString(o.Body)
	return b.String()
}

// narrowingNote warns, after a write that landed, when it dropped tags or keys
// the replaced version had. A failed check is logged and never fails the write.
func (t *Tools) narrowingNote(ctx context.Context, doc *docwrite.Doc, w merge.Write, status string) string {
	if !protocol.IsWriteSuccess(status) {
		return ""
	}
	note, err := metaguard.Gate(ctx, w.ExpectedVersion, w.Metadata, func(ctx context.Context) (fetch.Result, error) {
		return doc.FetchVersion(ctx, w.ExpectedVersion)
	})
	if err != nil {
		t.warnf("warning: publish metadata check mark://%s%s: %v", doc.Host, w.Path, err)
	}
	return note
}

// AppendArgs are mark_append's arguments; ExpectedVersion 0 resolves the
// current version through VERSIONS.
type AppendArgs struct {
	URL, Body       string
	ExpectedVersion int
}

// Append answers mark_append.
func (t *Tools) Append(ctx context.Context, args AppendArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	write, bad := t.writer(ctx, target, "append")
	if bad != nil {
		return *bad
	}
	if args.ExpectedVersion < 0 {
		return failure("expected_version must be >= 0")
	}
	result, err := t.doc(ctx, target, write).Append(ctx, docwrite.AppendRequest{
		Body: args.Body, ExpectedVersion: args.ExpectedVersion, Metadata: t.agentMeta(ctx),
	})
	if err != nil {
		return t.writeFailed(SiteAppend, target.Host, err)
	}
	return text(formatResult(&result, writeFields...))
}

// Archive answers mark_archive.
func (t *Tools) Archive(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	write, bad := t.writer(ctx, target, "archive")
	if bad != nil {
		return *bad
	}
	result, err := t.doc(ctx, target, write).Archive(ctx)
	if err != nil {
		return t.writeFailed(SiteArchive, target.Host, err)
	}
	return text(formatResult(&result, "version"))
}
