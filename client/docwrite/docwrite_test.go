package docwrite_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
)

func doc(backend docwrite.Backend) *docwrite.Doc {
	return &docwrite.Doc{
		Backend: backend, Host: "host:6309", Path: "/doc.md", ReadToken: "read-token",
		Write: docwrite.SendOnce("write-token"),
	}
}

func versionOf(r docwrite.Result) int { //nolint:gocritic // a test reads one result
	v, _ := strconv.Atoi(r.Response.Metadata["version"]) // absent reads as 0, which no case wants
	return v
}

func TestPublishModes(t *testing.T) {
	conflict := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict, Metadata: map[string]string{"server-version": "6"}}}, nil
	}
	backend := &fetchtest.Client{
		PublishFn: conflict,
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == "/doc.md/v5" {
				return fetchtest.Head("a\nb\nc\n", 5, nil), nil
			}
			return fetchtest.Head("a\nb\nC\n", 6, nil), nil
		},
	}
	w := merge.Write{Path: "/doc.md", Body: "a\nB\nc\n", ExpectedVersion: 5}

	merged, err := doc(backend).Publish(t.Context(), w, merge.OnConflictMerge)
	if err != nil || merged.Status != merge.OutcomeCandidate || merged.Body != "a\nB\nC\n" || merged.PublishAtVersion != 6 {
		t.Fatalf("merge mode = %+v, %v", merged, err)
	}
	failed, err := doc(backend).Publish(t.Context(), w, merge.OnConflictFail)
	if err != nil || failed.Status != merge.OutcomeOK || failed.Publish.Status != protocol.StatusConflict || failed.Publish.ServerVersion != 6 {
		t.Fatalf("fail mode = %+v, %v, want the conflict reported as is", failed, err)
	}
	sent := backend.PublishCalls[0]
	if sent.Host != "host:6309" || sent.Token != "write-token" || backend.FetchCalls[0].Token != "read-token" {
		t.Errorf("publish = %+v, first read = %+v", sent, backend.FetchCalls[0])
	}
}

// A write whose response was lost may have landed: look at the head, never resend.
func TestWritesReconcileAnUnknownOutcome(t *testing.T) {
	meta := map[string]string{"agent": "me"}
	tests := []struct {
		name        string
		call        func(*docwrite.Doc) (string, int, error)
		head        fetch.Result
		wantStatus  string
		wantVersion int
	}{
		{"publish landed", func(d *docwrite.Doc) (string, int, error) {
			o, err := d.Publish(t.Context(), merge.Write{Path: "/doc.md", Body: "mine", ExpectedVersion: 3, Metadata: meta}, merge.OnConflictFail)
			return o.Publish.Status, o.Publish.Version, err
		}, fetchtest.Head("mine", 4, meta), protocol.StatusOK, 4},
		{"publish not landed", func(d *docwrite.Doc) (string, int, error) {
			o, err := d.Publish(t.Context(), merge.Write{Path: "/doc.md", Body: "mine", ExpectedVersion: 3, Metadata: meta}, merge.OnConflictFail)
			return o.Publish.Status, o.Publish.Version, err
		}, fetchtest.Head("theirs", 4, nil), "", 0},
		{"append landed", func(d *docwrite.Doc) (string, int, error) {
			r, err := d.Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
			return r.Response.Status, versionOf(r), err
		}, fetchtest.Head("old\nmore", 4, meta), protocol.StatusOK, 4},
		{"append by someone else", func(d *docwrite.Doc) (string, int, error) {
			r, err := d.Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
			return r.Response.Status, versionOf(r), err
		}, fetchtest.Head("old\nmore", 4, map[string]string{"agent": "other"}), "", 0},
		{"archive landed", func(d *docwrite.Doc) (string, int, error) {
			r, err := d.Archive(t.Context())
			return r.Response.Status, versionOf(r), err
		}, fetch.Result{Response: protocol.Response{Status: protocol.StatusArchived, Metadata: map[string]string{"version": "4"}}}, protocol.StatusOK, 4},
		{"archive still live", func(d *docwrite.Doc) (string, int, error) {
			r, err := d.Archive(t.Context())
			return r.Response.Status, versionOf(r), err
		}, fetchtest.Head("live", 3, nil), "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lostWrite := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
				return fetch.Result{}, fetchtest.LostResponse()
			}
			backend := &fetchtest.Client{
				PublishFn: lostWrite, AppendFn: lostWrite,
				ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) {
					return fetch.Result{}, fetchtest.LostResponse()
				},
				FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) { return tt.head, nil },
			}
			status, version, err := tt.call(doc(backend))
			if tt.wantStatus == "" {
				if !errors.Is(err, fetch.ErrOutcomeUnknown) {
					t.Fatalf("err = %v, want the unknown outcome to stand", err)
				}
			} else if err != nil || status != tt.wantStatus || version != tt.wantVersion {
				t.Fatalf("got %q v%d, %v; want %q v%d reconciled", status, version, err, tt.wantStatus, tt.wantVersion)
			}
			if n := len(backend.PublishCalls) + len(backend.AppendCalls) + len(backend.ArchiveCalls); n != 1 {
				t.Errorf("writes sent = %d, want exactly one", n)
			}
		})
	}
}

// A write that never left has a known outcome and costs no probe.
func TestDefiniteFailuresDoNotProbe(t *testing.T) {
	refused := errors.New("dial refused")
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		AppendFn:  func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) { return fetch.Result{}, refused },
	}
	d := doc(backend)
	_, errPublish := d.Publish(t.Context(), merge.Write{Path: "/doc.md", Body: "x", ExpectedVersion: 3}, merge.OnConflictFail)
	_, errAppend := d.Append(t.Context(), docwrite.AppendRequest{Body: "x", ExpectedVersion: 3})
	_, errArchive := d.Archive(t.Context())
	for name, err := range map[string]error{"publish": errPublish, "append": errAppend, "archive": errArchive} {
		if !errors.Is(err, refused) {
			t.Errorf("%s err = %v", name, err)
		}
	}
	if len(backend.FetchCalls) != 0 {
		t.Errorf("head probes = %+v, want none", backend.FetchCalls)
	}
}

func TestAppendResolvesTheCurrentVersion(t *testing.T) {
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 7), nil
		},
	}
	got, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more"})
	if err != nil || got.Response.Status != protocol.StatusOK {
		t.Fatalf("Append = %+v, %v", got, err)
	}
	if backend.AppendCalls[0].ExpectedVersion != 7 || backend.AppendCalls[0].Token != "write-token" || backend.VersionsCalls[0].Token != "read-token" {
		t.Errorf("append = %+v, versions = %+v", backend.AppendCalls[0], backend.VersionsCalls[0])
	}

	backend.VersionsFn = func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	var unresolved *docwrite.VersionError
	if _, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more"}); !errors.As(err, &unresolved) || unresolved.Error() != "could not resolve version: not-found" {
		t.Errorf("err = %v, want a VersionError naming the status", err)
	}
}

// A refusal's body is the server saying what to fix; both modes must keep it.
func TestPublishKeepsTheServersExplanation(t *testing.T) {
	const why = "\n# Bad Request\n\npolicy: missing-tags\n"
	backend := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: why}}, nil
	}}
	for _, mode := range []string{merge.OnConflictMerge, merge.OnConflictFail} {
		got, err := doc(backend).Publish(t.Context(), merge.Write{Path: "/doc.md", Body: "x", ExpectedVersion: 1}, mode)
		if err != nil || got.Publish.Status != protocol.StatusBadRequest || got.Publish.Body != why {
			t.Errorf("%s mode = %+v, %v, want the refusal with its body", mode, got.Publish, err)
		}
	}
}
