package marktools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

func newTools(t *testing.T, backend marktools.Backend, hooks marktools.Hooks) *marktools.Tools {
	t.Helper()
	tools, err := marktools.New(backend, hooks)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func TestList(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, directHooks())

	got := tools.List(t.Context(), marktools.ListArgs{URL: "/docs/", IncludeArchived: true, Cursor: "b.md", PageSize: float64(25)})
	if got.IsError || got.Text != mcpfmt.Full(fetchtest.ListPage("/docs/", ""), "modified") {
		t.Errorf("List = %+v", got)
	}
	want := fetch.ListRequest{
		Host: "host:6309", Path: "/docs/", Token: "token-for-host:6309",
		IncludeArchived: true, Cursor: "b.md", PageSize: 25,
	}
	if len(backend.ListCalls) != 1 || backend.ListCalls[0] != want {
		t.Errorf("calls = %+v, want %+v", backend.ListCalls, want)
	}
}

func TestListRefusesBadArgumentsAndAStuckCursor(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, directHooks())
	for name, tt := range map[string]struct {
		args marktools.ListArgs
		want string
	}{
		"fraction":  {marktools.ListArgs{URL: "/", PageSize: 2.5}, "page_size must be an integer"},
		"string":    {marktools.ListArgs{URL: "/", PageSize: "ten"}, "page_size must be an integer"},
		"too small": {marktools.ListArgs{URL: "/", PageSize: 0}, "page_size must be between 1 and 1000"},
		"too large": {marktools.ListArgs{URL: "/", PageSize: protocol.MaxListPageSize + 1}, "page_size must be between 1 and 1000"},
		"bad url":   {marktools.ListArgs{URL: "bad", PageSize: "ten"}, "invalid URL: unsupported scheme"},
	} {
		if got := tools.List(t.Context(), tt.args); !got.IsError || got.Text != tt.want {
			t.Errorf("%s: got %+v, want %q", name, got, tt.want)
		}
	}
	if len(backend.ListCalls) != 0 {
		t.Error("refused arguments must not reach the backend")
	}

	backend.ListFn = func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		return fetchtest.ListPage(r.Path, r.Cursor, "a.md"), nil // next-cursor repeats the one sent
	}
	got := tools.List(t.Context(), marktools.ListArgs{URL: "/", Cursor: "a.md"})
	if !got.IsError || got.Text != "list failed: continuation cursor is missing or did not advance" {
		t.Errorf("stuck cursor = %+v", got)
	}
}

func TestLookup(t *testing.T) {
	rows := []render.LookupRow{{Path: "/docs/a.md", Importance: 0.5, Title: "A", Tags: []string{"x"}}}
	backend := &fetchtest.Client{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			return fetchtest.Lookup(r.Query, r.Scope, "", rows...), nil // a catalog answer to a body request
		},
	}
	tools := newTools(t, backend, directHooks())
	args := marktools.LookupArgs{URL: "/docs/", Query: "alpha", Filter: "tag=x", Limit: 3, Match: fetch.MatchBody, Render: mcpfmt.Options{Envelope: &mcpfmt.Lookup}}

	got := tools.Lookup(t.Context(), args)
	wantReq := fetch.LookupRequest{
		Host: "host:6309", Scope: "/docs/", Token: "token-for-host:6309",
		Query: "alpha", Filter: "tag=x", Limit: 3, Match: fetch.MatchBody,
	}
	if len(backend.LookupCalls) != 1 || backend.LookupCalls[0] != wantReq {
		t.Fatalf("calls = %+v, want %+v", backend.LookupCalls, wantReq)
	}
	want := mcpfmt.Format(fetchtest.Lookup("alpha", "/docs/", "", rows...), args.Render) + mcpfmt.Note(fetch.CatalogFallbackNote)
	if got.IsError || got.Text != want {
		t.Errorf("Lookup = %q\nwant     %q", got.Text, want)
	}
}

// Expansion reads each matched document with the token the lookup used.
func TestLookupExpandsWithinTheBudget(t *testing.T) {
	rows := []render.LookupRow{{Path: "/docs/a.md", Anchor: "intro", Importance: 0.5, Title: "A"}}
	backend := &fetchtest.Client{
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			return fetchtest.Lookup(r.Query, r.Scope, fetch.MatchBody, rows...), nil
		},
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# A\n\n## Intro\n\nalpha text\n"}}, nil
		},
	}
	tools := newTools(t, backend, directHooks())
	got := tools.Lookup(t.Context(), marktools.LookupArgs{URL: "/", Query: "alpha", Match: fetch.MatchBody, Budget: 1500, Render: mcpfmt.Options{Envelope: &mcpfmt.Lookup}})
	if got.IsError || !strings.Contains(got.Text, ">>> /docs/a.md#intro") || !strings.Contains(got.Text, "alpha text") {
		t.Errorf("expanded lookup = %q", got.Text)
	}
	if len(backend.FetchCalls) != 1 || backend.FetchCalls[0].Token != "token-for-host:6309" {
		t.Errorf("expansion fetches = %+v, want one with the lookup's token", backend.FetchCalls)
	}
}

func TestArchive(t *testing.T) {
	backend := &fetchtest.Client{}
	hooks := directHooks()
	var verbs []string
	hooks.Writer = func(_ context.Context, target marktools.Target, verb string) (marktools.WriteFunc, error) {
		verbs = append(verbs, verb+" "+target.Path)
		if target.Path == "/locked.md" {
			return nil, errors.New("archive requires a token")
		}
		return docwrite.SendOnce("write-token"), nil
	}
	tools := newTools(t, backend, hooks)

	got := tools.Archive(t.Context(), "/doc.md")
	wantText := mcpfmt.Full(fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "3", "archived": "true"}}}, "version")
	if got.IsError || got.Text != wantText {
		t.Errorf("Archive = %+v, want %q", got, wantText)
	}
	want := fetch.ArchiveRequest{Host: "host:6309", Path: "/doc.md", Token: "write-token"}
	if len(backend.ArchiveCalls) != 1 || backend.ArchiveCalls[0] != want {
		t.Errorf("calls = %+v, want %+v", backend.ArchiveCalls, want)
	}

	// The surface's refusal is the tool's answer, word for word, and nothing is sent.
	if got := tools.Archive(t.Context(), "/locked.md"); !got.IsError || got.Text != "archive requires a token" {
		t.Errorf("refused = %+v", got)
	}
	if len(backend.ArchiveCalls) != 1 || strings.Join(verbs, ",") != "archive /doc.md,archive /locked.md" {
		t.Errorf("calls = %d, verbs = %v", len(backend.ArchiveCalls), verbs)
	}

	noWriter := newTools(t, backend, directHooks())
	if got := noWriter.Archive(t.Context(), "/doc.md"); !got.IsError || !strings.Contains(got.Text, "no writer") {
		t.Errorf("a surface without a Writer must refuse writes, got %+v", got)
	}
}

func TestDiscover(t *testing.T) {
	manifest := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Agent Manifest\n", Metadata: map[string]string{"version": "2"}}}
	backend := &fetchtest.Client{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) { return manifest, nil },
	}
	hooks := directHooks()
	hooks.Resolve = func(_ context.Context, raw string) (marktools.Target, error) {
		if raw == "" {
			return marktools.Target{}, marktools.Verbatim("no server specified: provide a URL or set -host")
		}
		return marktools.Target{Host: "host:6309", Path: raw}, nil
	}
	tools := newTools(t, backend, hooks)

	got := tools.Discover(t.Context(), "/some/doc.md")
	if got.IsError || got.Text != mcpfmt.Full(manifest, "version", "modified") {
		t.Errorf("Discover = %+v", got)
	}
	// Whatever the URL names, discovery reads the server's manifest, and with the
	// host's token like any other read: a server may require one.
	want := fetch.FetchRequest{Host: "host:6309", Path: protocol.WellKnownManifestPath, Token: "token-for-host:6309"}
	if len(backend.FetchCalls) != 1 || backend.FetchCalls[0] != want {
		t.Errorf("calls = %+v, want %+v", backend.FetchCalls, want)
	}
	if got := tools.Discover(t.Context(), ""); !got.IsError || got.Text != "no server specified: provide a URL or set -host" {
		t.Errorf("no server = %+v", got)
	}
}
