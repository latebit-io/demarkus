package marktools_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// writingHooks is a direct client with a write token and a named agent.
func writingHooks() marktools.Hooks {
	hooks := directHooks()
	hooks.Agent = func(context.Context) string { return "test-agent" }
	hooks.Writer = func(_ context.Context, target marktools.Target, verb string) (marktools.WriteFunc, error) {
		if target.Path == "/locked.md" {
			return nil, errors.New(verb + " requires a token")
		}
		return docwrite.SendOnce("write-token"), nil
	}
	return hooks
}

func TestPublishSendsTheWriteWithIdentity(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, writingHooks())
	got := tools.Publish(t.Context(), marktools.PublishArgs{
		URL: "/doc.md", Body: "# Doc\n", ExpectedVersion: new(0), OnConflict: "fail",
		Metadata: map[string]any{"tags": "a,b", "importance": 0.5, "agent": "someone-else"},
	})
	want := mcpfmt.Full(fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "1"}}}, "version", "modified", "server-version")
	if got.IsError || got.Text != want {
		t.Fatalf("Publish = %+v, want %q", got, want)
	}
	if len(backend.PublishCalls) != 1 {
		t.Fatalf("publishes = %d", len(backend.PublishCalls))
	}
	sent := backend.PublishCalls[0]
	if sent.Host != "host:6309" || sent.Path != "/doc.md" || sent.Token != "write-token" || sent.Body != "# Doc\n" || sent.ExpectedVersion != 0 {
		t.Errorf("sent = %+v", sent)
	}
	// Identity is the surface's to set: a caller cannot write under another name.
	if fmt.Sprint(sent.Metadata) != "map[agent:test-agent importance:0.5 tags:a,b]" {
		t.Errorf("metadata = %v", sent.Metadata)
	}
}

// Arguments are checked before authorization, authorization before any write:
// a caller fixes what it sent before learning it may not write at all.
func TestPublishChecksInOrder(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, writingHooks())
	for name, tt := range map[string]struct {
		args marktools.PublishArgs
		want string
	}{
		"bad url first":          {marktools.PublishArgs{URL: "bad"}, "invalid URL: unsupported scheme"},
		"then the version":       {marktools.PublishArgs{URL: "/locked.md"}, "expected_version is required"},
		"negative version":       {marktools.PublishArgs{URL: "/locked.md", ExpectedVersion: new(-1)}, "expected_version must be >= 0"},
		"then the conflict mode": {marktools.PublishArgs{URL: "/locked.md", ExpectedVersion: new(1), OnConflict: "overwrite"}, `invalid on_conflict "overwrite": expected "merge" or "fail"`},
		"then authorization":     {marktools.PublishArgs{URL: "/locked.md", ExpectedVersion: new(1)}, "publish requires a token"},
	} {
		if got := tools.Publish(t.Context(), tt.args); !got.IsError || got.Text != tt.want {
			t.Errorf("%s: got %+v, want %q", name, got, tt.want)
		}
	}
	if len(backend.PublishCalls) != 0 {
		t.Error("a refused publish must not reach the backend")
	}
}

func TestAppendChecksItsVersionBeforeAuthorizing(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, writingHooks())
	got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/locked.md", Body: "x", ExpectedVersion: -1})
	if !got.IsError || got.Text != "expected_version must be >= 0" {
		t.Errorf("bad version = %+v", got)
	}
	got = tools.Append(t.Context(), marktools.AppendArgs{URL: "/locked.md", Body: "x"})
	if !got.IsError || got.Text != "append requires a token" {
		t.Errorf("refused = %+v", got)
	}
	if n := len(backend.VersionsCalls) + len(backend.AppendCalls); n != 0 {
		t.Errorf("backend calls = %d, want none before authorization", n)
	}
}

func TestPublishConflictOffersAMergeCandidate(t *testing.T) {
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict, Metadata: map[string]string{"server-version": "6"}}}, nil
		},
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == "/doc.md/v5" {
				return fetchtest.Head("a\nb\nc\n", 5, nil), nil
			}
			return fetchtest.Head("a\nb\nC\n", 6, nil), nil
		},
	}
	tools := newTools(t, backend, writingHooks())
	got := tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: "a\nB\nc\n", ExpectedVersion: new(5)})
	want := "status: merge-candidate\nyour-version: 5\ncurrent-version: 6\npublish-at-version: 6\nhas-markers: false\n\na\nB\nC\n"
	if got.IsError || got.Text != want {
		t.Errorf("merge candidate = %q\nwant %q", got.Text, want)
	}
	for _, r := range backend.FetchCalls {
		if r.Token != "token-for-host:6309" {
			t.Errorf("merge read %s sent token %q, want the read token", r.Path, r.Token)
		}
	}
}

// A write whose response was lost may have landed. The tool checks the head
// and never invites a blind resend (the self conflict debt, C4's leftover).
func TestWritesReconcileAfterAnUnknownOutcome(t *testing.T) {
	sentMeta := map[string]string{"agent": "test-agent"}
	tests := []struct {
		name     string
		call     func(*marktools.Tools) marktools.Result
		head     fetch.Result
		wantText string
		wantErr  bool
	}{
		{
			name: "publish fail mode, landed",
			call: func(tools *marktools.Tools) marktools.Result {
				return tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: "mine", ExpectedVersion: new(3), OnConflict: "fail"})
			},
			head: fetchtest.Head("mine", 4, sentMeta), wantText: "status: ok\nversion: 4\n",
		},
		{
			name: "publish fail mode, not landed",
			call: func(tools *marktools.Tools) marktools.Result {
				return tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: "mine", ExpectedVersion: new(3), OnConflict: "fail"})
			},
			head: fetchtest.Head("theirs", 4, nil), wantErr: true,
			wantText: "publish failed: read response: request sent but outcome unknown; it may have landed: fetch the document before retrying",
		},
		{
			name: "append, landed",
			call: func(tools *marktools.Tools) marktools.Result {
				return tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more", ExpectedVersion: 3})
			},
			head: fetchtest.Head("old\nmore", 4, sentMeta), wantText: "status: ok\nversion: 4\n",
		},
		{
			name: "append, someone else wrote",
			call: func(tools *marktools.Tools) marktools.Result {
				return tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more", ExpectedVersion: 3})
			},
			head: fetchtest.Head("old\nother", 4, sentMeta), wantErr: true,
			wantText: "append failed: read response: request sent but outcome unknown; it may have landed: fetch the document before retrying",
		},
		{
			name: "archive, landed",
			call: func(tools *marktools.Tools) marktools.Result { return tools.Archive(t.Context(), "/doc.md") },
			head: fetchtest.Archived(),
			// An archived document answers without a version, so none is claimed.
			wantText: "status: ok\narchived: true\n",
		},
		{
			name: "archive, still live",
			call: func(tools *marktools.Tools) marktools.Result { return tools.Archive(t.Context(), "/doc.md") },
			head: fetchtest.Head("live", 3, nil), wantErr: true,
			wantText: "archive failed: read response: request sent but outcome unknown; it may have landed: fetch the document before retrying",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lost := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
				return fetch.Result{}, fetchtest.LostResponse()
			}
			backend := &fetchtest.Client{
				PublishFn: lost, AppendFn: lost,
				ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) {
					return fetch.Result{}, fetchtest.LostResponse()
				},
				FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
					if r.Path == "/doc.md/v3" {
						return fetchtest.Head("old", 3, nil), nil // the base every write here started from
					}
					return tt.head, nil
				},
			}
			got := tt.call(newTools(t, backend, writingHooks()))
			if got.IsError != tt.wantErr || got.Text != tt.wantText {
				t.Errorf("got %+v\nwant error=%v text %q", got, tt.wantErr, tt.wantText)
			}
			if n := len(backend.PublishCalls) + len(backend.AppendCalls) + len(backend.ArchiveCalls); n != 1 {
				t.Errorf("writes sent = %d, want exactly one: never resend", n)
			}
		})
	}
}

func TestAppendResolvesTheVersionItself(t *testing.T) {
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 7), nil
		},
	}
	tools := newTools(t, backend, writingHooks())
	got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more"})
	if got.IsError || !strings.HasPrefix(got.Text, "status: ok") {
		t.Fatalf("Append = %+v", got)
	}
	if len(backend.AppendCalls) != 1 || backend.AppendCalls[0].ExpectedVersion != 7 || backend.AppendCalls[0].Token != "write-token" ||
		backend.AppendCalls[0].Metadata["agent"] != "test-agent" {
		t.Errorf("append = %+v", backend.AppendCalls)
	}
	if len(backend.VersionsCalls) != 1 || backend.VersionsCalls[0].Token != "token-for-host:6309" {
		t.Errorf("versions = %+v, want the resolve to read with the read token", backend.VersionsCalls)
	}

	backend.VersionsFn = func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more"}); !got.IsError || got.Text != "could not resolve version: not-found" {
		t.Errorf("unresolvable = %+v", got)
	}
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more", ExpectedVersion: -2}); got.Text != "expected_version must be >= 0" {
		t.Errorf("negative = %+v", got)
	}
}

// A write that never left (a refused dial) has a known outcome: no probe.
func TestDefiniteWriteFailuresDoNotProbeTheHead(t *testing.T) {
	refused := errors.New("dial refused")
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		AppendFn:  func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) { return fetch.Result{}, refused },
	}
	tools := newTools(t, backend, writingHooks())
	for name, got := range map[string]marktools.Result{
		"publish": tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: "x", ExpectedVersion: new(3), OnConflict: "fail"}),
		"append":  tools.Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "x", ExpectedVersion: 3}),
		"archive": tools.Archive(t.Context(), "/doc.md"),
	} {
		if !got.IsError || got.Text != name+" failed: dial refused" {
			t.Errorf("%s = %+v", name, got)
		}
	}
	if len(backend.FetchCalls) != 0 {
		t.Errorf("head probes = %+v, want none", backend.FetchCalls)
	}
}

// A policy refusal is only useful with its reason: the agent has to know what
// to fix. The default merge mode used to drop the server's body.
func TestPublishRefusalShowsTheServersReason(t *testing.T) {
	const why = "\n# Bad Request\n\npolicy: missing-tags\n"
	backend := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: why}}, nil
	}}
	tools := newTools(t, backend, writingHooks())
	for _, mode := range []string{"", "fail"} {
		got := tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: "x", ExpectedVersion: new(1), OnConflict: mode})
		if got.IsError || !strings.HasPrefix(got.Text, "status: bad-request\n") || !strings.Contains(got.Text, "policy: missing-tags") {
			t.Errorf("on_conflict=%q: %q, want the status and the server's reason", mode, got.Text)
		}
	}
}
