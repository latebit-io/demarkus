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
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
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

// Result is a write the server answered, or one found at the head after its
// response was lost: then Response is ok at the head's version, all that is known.
type Result struct {
	Response   protocol.Response
	Reconciled bool
}

// SendOnce is the WriteFunc of a surface with one fixed token and no retry.
func SendOnce(token string) WriteFunc {
	return func(_ context.Context, op func(token string) (fetch.Result, error)) (fetch.Result, error) {
		return op(token)
	}
}

// Publish sends w. mode is merge.OnConflictMerge or merge.OnConflictFail: a
// conflict comes back as a candidate to review, or as the conflict status.
func (d *Doc) Publish(ctx context.Context, w merge.Write, mode string) (merge.Outcome, error) {
	if w.ExpectedVersion < 0 {
		return merge.Outcome{}, merge.ErrInvalidExpectedVersion
	}
	if mode == merge.OnConflictMerge {
		return merge.Candidate(ctx, (*mergeClient)(d), w)
	}
	published, err := (*mergeClient)(d).Publish(ctx, w)
	if err != nil {
		// Sent with its response lost: it may have landed, so look, never resend.
		if errors.Is(err, fetch.ErrOutcomeUnknown) {
			if head, ok := merge.Landed(ctx, (*mergeClient)(d), w); ok {
				return merge.Outcome{Status: merge.OutcomeOK, Publish: merge.Reconciled(head)}, nil
			}
		}
		return merge.Outcome{}, err
	}
	return merge.Outcome{Status: merge.OutcomeOK, Publish: published}, nil
}

// PublishUnchecked replaces the document whatever its version: the caller has
// said it does not care what is there. Nothing to merge, nothing to reconcile.
func (d *Doc) PublishUnchecked(ctx context.Context, body string, meta map[string]string) (Result, error) {
	r, err := d.Write(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Publish(ctx, fetch.WriteRequest{
			Host: d.Host, Path: d.Path, Token: token, Body: body, ExpectedVersion: -1, Metadata: meta,
		})
	})
	if err != nil {
		return Result{}, err
	}
	return answered(r)
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

// Append adds to the document. Its reconcile accepts a head at the next
// version that ends in the appended body under the same agent.
func (d *Doc) Append(ctx context.Context, req AppendRequest) (Result, error) {
	if req.ExpectedVersion < 0 {
		return Result{}, merge.ErrInvalidExpectedVersion
	}
	expected := req.ExpectedVersion
	if expected == 0 {
		var err error
		if expected, err = d.CurrentVersion(ctx); err != nil {
			return Result{}, err
		}
	}
	r, err := d.Write(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Append(ctx, fetch.WriteRequest{
			Host: d.Host, Path: d.Path, Token: token,
			Body: req.Body, ExpectedVersion: expected, Metadata: req.Metadata,
		})
	})
	if err == nil {
		return answered(r)
	}
	if errors.Is(err, fetch.ErrOutcomeUnknown) {
		head, probeErr := (*mergeClient)(d).FetchCurrent(ctx, d.Path)
		if probeErr == nil && head.Status == protocol.StatusOK && head.Version == expected+1 &&
			strings.HasSuffix(head.Body, req.Body) && head.Metadata["agent"] == req.Metadata["agent"] {
			return reconciledAt(head.Version), nil
		}
	}
	return Result{}, err
}

// Archive archives the document. Archived is a state, not an event, so an
// archived head settles a lost response whoever archived it.
func (d *Doc) Archive(ctx context.Context) (Result, error) {
	r, err := d.Write(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Archive(ctx, fetch.ArchiveRequest{Host: d.Host, Path: d.Path, Token: token})
	})
	if err == nil {
		return answered(r)
	}
	if errors.Is(err, fetch.ErrOutcomeUnknown) {
		if head, probeErr := (*mergeClient)(d).FetchCurrent(ctx, d.Path); probeErr == nil && head.Status == protocol.StatusArchived {
			result := reconciledAt(head.Version)
			result.Response.Metadata["archived"] = "true" // as the server answers an ARCHIVE
			return result, nil
		}
	}
	return Result{}, err
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
	return d.Backend.Fetch(ctx, fetch.FetchRequest{Host: d.Host, Path: generation.VersionPath(d.Path, version), Token: d.ReadToken})
}

// answered refuses a malformed version, as the merge path does.
func answered(r fetch.Result) (Result, error) {
	if _, err := optionalInt(r.Response.Metadata, "version"); err != nil {
		return Result{}, err
	}
	return Result{Response: r.Response}, nil
}

// reconciledAt answers as the server would have: a landed write is ok.
func reconciledAt(version int) Result {
	return Result{Reconciled: true, Response: protocol.Response{
		Status: protocol.StatusOK, Metadata: map[string]string{"version": strconv.Itoa(version)},
	}}
}

// mergeClient is Doc as the merge package's client: reads carry the read
// token, the publish goes through the surface's WriteFunc.
type mergeClient Doc

func (d *mergeClient) fetch(ctx context.Context, path string) (merge.Doc, error) {
	r, err := d.Backend.Fetch(ctx, fetch.FetchRequest{Host: d.Host, Path: path, Token: d.ReadToken})
	if err != nil {
		return merge.Doc{}, err
	}
	v, err := optionalInt(r.Response.Metadata, "version")
	if err != nil {
		return merge.Doc{}, err
	}
	return merge.Doc{Status: r.Response.Status, Body: r.Response.Body, Version: v, Metadata: r.Response.Metadata}, nil
}

func (d *mergeClient) FetchVersion(ctx context.Context, path string, version int) (merge.Doc, error) {
	return d.fetch(ctx, generation.VersionPath(path, version))
}

func (d *mergeClient) FetchCurrent(ctx context.Context, path string) (merge.Doc, error) {
	return d.fetch(ctx, path)
}

func (d *mergeClient) Publish(ctx context.Context, w merge.Write) (merge.PublishResult, error) {
	r, err := d.Write(ctx, func(token string) (fetch.Result, error) {
		return d.Backend.Publish(ctx, fetch.WriteRequest{
			Host: d.Host, Path: w.Path, Token: token,
			Body: w.Body, ExpectedVersion: w.ExpectedVersion, Metadata: w.Metadata,
		})
	})
	if err != nil {
		return merge.PublishResult{}, err
	}
	v, err := optionalInt(r.Response.Metadata, "version")
	if err != nil {
		return merge.PublishResult{}, err
	}
	sv, err := optionalInt(r.Response.Metadata, "server-version")
	if err != nil {
		return merge.PublishResult{}, err
	}
	return merge.PublishResult{Status: r.Response.Status, Version: v, ServerVersion: sv, Metadata: r.Response.Metadata, Body: r.Response.Body}, nil
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
