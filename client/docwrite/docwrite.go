// Package docwrite is the write contract every surface shares: publish with a
// merge candidate on conflict, append with version resolution, archive, and
// for all three a look at the head, never a resend, when a response is lost.
// It returns outcomes; a surface words them (tool text, CLI output, exit code).
package docwrite

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// Backend is the part of the protocol client a write needs.
type Backend interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error)
}

// WriteFunc runs op with a write token. A surface that mints tokens may run it
// again with a fresh one when the server refuses the first.
type WriteFunc func(ctx context.Context, op func(token string) (fetch.Result, error)) (fetch.Result, error)

// Doc is one document on one server: where reads and writes for it go.
type Doc struct {
	Backend    Backend
	Host, Path string
	ReadToken  string // sent on reads; "" sends none
	Write      WriteFunc
}

// Result is how a write ended: answered by the server, found at the head after
// its response was lost (Response is then ok at the head's version, all that is
// known), or refused with a merge Candidate to review.
type Result struct {
	Response   protocol.Response
	Reconciled bool
	Candidate  *Candidate
}

// StatusCandidate is how a surface names a Result that carries a Candidate.
const StatusCandidate = "merge-candidate"

// Candidate is a three way merge offered after a version conflict. It is never
// published: disjoint lines are not disjoint meaning, so the caller reviews it
// and republishes at PublishAtVersion.
type Candidate struct {
	Body             string
	HasMarkers       bool
	BaseVersion      int
	TheirVersion     int
	PublishAtVersion int
}

// Write is one PUBLISH. ExpectedVersion 0 creates only; a conflict in that
// mode merges against an empty base.
type Write struct {
	Body            string
	ExpectedVersion int
	Metadata        map[string]string
}

// ErrInvalidExpectedVersion refuses a negative version on any write.
var ErrInvalidExpectedVersion = errors.New("expected_version must be >= 0")

// SendOnce is the WriteFunc of a surface with one fixed token and no retry.
func SendOnce(token string) WriteFunc {
	return func(_ context.Context, op func(token string) (fetch.Result, error)) (fetch.Result, error) {
		return op(token)
	}
}

// Publish sends w once. mode is merge.OnConflictMerge or merge.OnConflictFail:
// a version conflict comes back as a Candidate, or as the conflict status.
func (d *Doc) Publish(ctx context.Context, w Write, mode string) (Result, error) {
	if w.ExpectedVersion < 0 {
		return Result{}, ErrInvalidExpectedVersion
	}
	result, err := d.send(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Publish(ctx, fetch.WriteRequest{
			Host: d.Host, Path: d.Path, Token: token,
			Body: w.Body, ExpectedVersion: w.ExpectedVersion, Metadata: w.Metadata,
		})
	}, probe{path: protocol.VersionPath(d.Path, w.ExpectedVersion+1), landed: func(written *headDoc) (bool, error) {
		return w.landedAt(written), nil
	}})
	if err != nil {
		if mode == merge.OnConflictMerge {
			// Tool text in this mode has always named the step that failed.
			err = fmt.Errorf("publish: %w", err)
		}
		return Result{}, err
	}
	if result.Response.Status != protocol.StatusConflict || mode != merge.OnConflictMerge {
		return result, nil
	}
	return d.candidate(ctx, w, result)
}

// candidate answers a conflict with a merge of w into the head, based on the
// version w was edited from.
func (d *Doc) candidate(ctx context.Context, w Write, conflict Result) (Result, error) { //nolint:gocritic // the conflict is returned as is when there is no base
	head, err := d.head(ctx, d.Path)
	if err != nil {
		return Result{}, fmt.Errorf("fetch current: %w", err)
	}
	if head.status != protocol.StatusOK {
		return Result{}, fmt.Errorf("fetch current: status %s", head.status)
	}
	// A conflict against our own earlier attempt is a success, not a merge.
	if w.landedAt(&head) {
		return reconciledAt(head.version), nil
	}
	// Without a real head version the follow up publish would be create only.
	if head.version <= 0 {
		return Result{}, errors.New("fetch current: missing or invalid version metadata")
	}
	base := ""
	if w.ExpectedVersion > 0 {
		baseDoc, err := d.head(ctx, protocol.VersionPath(d.Path, w.ExpectedVersion))
		if err != nil {
			return Result{}, fmt.Errorf("fetch base v%d: %w", w.ExpectedVersion, err)
		}
		// No base: it never existed, or retention pruned it. Nothing to merge
		// against, so the server's conflict is the answer.
		if baseDoc.status == protocol.StatusNotFound {
			return conflict, nil
		}
		if baseDoc.status != protocol.StatusOK {
			return Result{}, fmt.Errorf("fetch base v%d: status %s", w.ExpectedVersion, baseDoc.status)
		}
		base = baseDoc.body
	}
	merged := merge.Diff3(base, w.Body, head.body)
	return Result{Candidate: &Candidate{
		Body: merged.Body, HasMarkers: merged.Conflict,
		BaseVersion: w.ExpectedVersion, TheirVersion: head.version, PublishAtVersion: head.version,
	}}, nil
}

// landedAt is whether the head is exactly what w submitted: its body at the
// next version, under every metadata key it sent.
func (w *Write) landedAt(head *headDoc) bool {
	return head.status == protocol.StatusOK && head.version == w.ExpectedVersion+1 &&
		head.body == w.Body && head.carries(w.Metadata)
}

// PublishUnchecked replaces the document whatever its version: the caller has
// said it does not care what is there. Nothing to merge, nothing to reconcile.
func (d *Doc) PublishUnchecked(ctx context.Context, body string, meta map[string]string) (Result, error) {
	r, err := d.Write(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Publish(ctx, fetch.WriteRequest{
			Host: d.Host, Path: d.Path, Token: token, Body: body, ExpectedVersion: -1, Metadata: meta,
		})
	})
	return Result{Response: r.Response}, err
}

// AppendRequest is one APPEND; ExpectedVersion 0 resolves the head first.
type AppendRequest struct {
	Body            string
	ExpectedVersion int
	Metadata        map[string]string
}

// VersionError is an APPEND that could not learn the version to append to.
type VersionError struct{ Reason string }

func (e *VersionError) Error() string { return "could not resolve version: " + e.Reason }

// Append adds to the document. It landed when the next version is exactly the
// base with the addition joined as the protocol joins them, under every
// metadata key that was sent.
func (d *Doc) Append(ctx context.Context, req AppendRequest) (Result, error) {
	if req.ExpectedVersion < 0 {
		return Result{}, ErrInvalidExpectedVersion
	}
	expected := req.ExpectedVersion
	if expected == 0 {
		var err error
		if expected, err = d.CurrentVersion(ctx); err != nil {
			return Result{}, err
		}
	}
	return d.send(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Append(ctx, fetch.WriteRequest{
			Host: d.Host, Path: d.Path, Token: token,
			Body: req.Body, ExpectedVersion: expected, Metadata: req.Metadata,
		})
	}, probe{path: protocol.VersionPath(d.Path, expected+1), landed: func(written *headDoc) (bool, error) {
		// The cheap checks first: the base is read only for a likely match.
		if written.status != protocol.StatusOK || written.version != expected+1 ||
			!strings.HasSuffix(written.body, req.Body) || !written.carries(req.Metadata) {
			return false, nil
		}
		return d.isAppendOf(ctx, written, expected, req.Body)
	}})
}

// Archive archives the document. Archived is a state, not an event, so an
// archived head settles a lost response whoever archived it.
func (d *Doc) Archive(ctx context.Context) (Result, error) {
	result, err := d.send(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Archive(ctx, fetch.ArchiveRequest{Host: d.Host, Path: d.Path, Token: token})
	}, probe{path: d.Path, landed: func(head *headDoc) (bool, error) {
		return head.status == protocol.StatusArchived, nil
	}})
	if err == nil && result.Reconciled {
		result.Response.Metadata["archived"] = "true" // as the server answers an ARCHIVE
	}
	return result, err
}

// probe is where to look for a write whose response was lost, and what it
// looks like there. PUBLISH and APPEND look at the version they would have
// created, which no later writer can change; ARCHIVE is a state of the head.
type probe struct {
	path   string
	landed func(found *headDoc) (bool, error)
}

// send runs one write through the surface's WriteFunc, once. When its response
// is lost the write may have landed, so it is looked for, never resent.
func (d *Doc) send(ctx context.Context, op func(token string) (fetch.Result, error), look probe) (Result, error) {
	r, err := d.Write(ctx, op)
	if err == nil {
		// Answered is answered: the response is passed on as the server wrote it.
		return Result{Response: r.Response}, nil
	}
	if errors.Is(err, protocol.ErrOutcomeUnknown) {
		found, probeErr := d.head(ctx, look.path)
		if probeErr != nil {
			// The outcome stays unknown; why the look did not help is said too.
			return Result{}, fmt.Errorf("%w; reconcile: %w", err, probeErr)
		}
		landed, probeErr := look.landed(&found)
		if probeErr != nil {
			return Result{}, fmt.Errorf("%w; reconcile: %w", err, probeErr)
		}
		if landed {
			return reconciledAt(found.version), nil
		}
	}
	return Result{}, err
}

// isAppendOf is whether written is the base version with addition appended. A
// suffix alone would also match a competing write that ends in the same words.
// A base that is gone (retention) proves nothing, so nothing is claimed.
func (d *Doc) isAppendOf(ctx context.Context, written *headDoc, baseVersion int, addition string) (bool, error) {
	base, err := d.head(ctx, protocol.VersionPath(d.Path, baseVersion))
	if err != nil {
		return false, fmt.Errorf("fetch base v%d: %w", baseVersion, err)
	}
	if base.status != protocol.StatusOK {
		return false, nil
	}
	want, err := storefmt.JoinContent([]byte(base.body), []byte(addition))
	if err != nil {
		return false, fmt.Errorf("join append content: %w", err)
	}
	return written.body == string(want), nil
}

// reconciledAt answers as the server would have: a landed write is ok. The
// version is given only when the probe learned one: an archived document
// answers a FETCH without it, and an invented 0 would be a lie.
func reconciledAt(version int) Result {
	meta := map[string]string{}
	if version > 0 {
		meta["version"] = strconv.Itoa(version)
	}
	return Result{Reconciled: true, Response: protocol.Response{Status: protocol.StatusOK, Metadata: meta}}
}

// CurrentVersion asks VERSIONS for the head, as a direct client would by hand.
func (d *Doc) CurrentVersion(ctx context.Context) (int, error) {
	result, err := d.Backend.Versions(ctx, fetch.VersionsRequest{Host: d.Host, Path: d.Path, Token: d.ReadToken})
	if err != nil {
		return 0, &VersionError{Reason: err.Error()}
	}
	if result.Response.Status != protocol.StatusOK {
		return 0, &VersionError{Reason: result.Response.Status}
	}
	cur, ok := result.Response.Metadata["current"]
	if !ok {
		return 0, &VersionError{Reason: "no current version in response"}
	}
	version, err := strconv.Atoi(cur)
	if err != nil {
		return 0, &VersionError{Reason: fmt.Sprintf("invalid current version %q", cur)}
	}
	return version, nil
}

// FetchVersion reads one historical version of the document.
func (d *Doc) FetchVersion(ctx context.Context, version int) (fetch.Result, error) {
	return d.Backend.Fetch(ctx, fetch.FetchRequest{Host: d.Host, Path: protocol.VersionPath(d.Path, version), Token: d.ReadToken})
}

// headDoc is a document as a read of it answered.
type headDoc struct {
	status, body string
	version      int
	metadata     map[string]string
}

func (d *Doc) head(ctx context.Context, path string) (headDoc, error) {
	r, err := d.Backend.Fetch(ctx, fetch.FetchRequest{Host: d.Host, Path: path, Token: d.ReadToken})
	if err != nil {
		return headDoc{}, err
	}
	version, err := optionalInt(r.Response.Metadata, "version")
	if err != nil {
		return headDoc{}, err
	}
	return headDoc{status: r.Response.Status, body: r.Response.Body, version: version, metadata: r.Response.Metadata}, nil
}

// carries is whether the document holds every submitted key, so another
// writer's identical body under different metadata is not mistaken for ours.
func (h *headDoc) carries(submitted map[string]string) bool {
	for k, v := range submitted {
		// An absent key is a mismatch even when the submitted value is empty.
		actual, ok := h.metadata[k]
		if !ok || strings.TrimSpace(actual) != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}

// optionalInt reads an integer metadata value: absent is 0, malformed is an
// error, so server side corruption surfaces instead of reading as zero.
func optionalInt(meta map[string]string, key string) (int, error) {
	s, ok := meta[key]
	if !ok || s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("response metadata %q = %q: %w", key, s, err)
	}
	return n, nil
}
