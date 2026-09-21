package marktools_test

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// directHooks resolve the way a direct client does: host:port from the URL
// and a token per host.
func directHooks() marktools.Hooks {
	return marktools.Hooks{
		Resolve: func(_ context.Context, raw string) (marktools.Target, error) {
			if raw == "bad" {
				return marktools.Target{}, errors.New("unsupported scheme")
			}
			return marktools.Target{Host: "host:6309", Path: raw}, nil
		},
		ReadToken: func(_ context.Context, host string) string { return "token-for-" + host },
	}
}

func TestVersions(t *testing.T) {
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 3), nil
		},
	}
	tools, err := marktools.New(backend, directHooks())
	if err != nil {
		t.Fatal(err)
	}

	got := tools.Versions(t.Context(), "/doc.md")
	want := mcpfmt.Full(fetchtest.Versions("/doc.md", 3), "total", "current", "chain-valid", "chain-error")
	if got.IsError || got.Text != want {
		t.Errorf("Versions = %+v\nwant text %q", got, want)
	}
	wantCall := fetch.VersionsRequest{Host: "host:6309", Path: "/doc.md", Token: "token-for-host:6309"}
	if len(backend.VersionsCalls) != 1 || backend.VersionsCalls[0] != wantCall {
		t.Errorf("calls = %+v, want %+v", backend.VersionsCalls, wantCall)
	}
}

func TestVersionsErrors(t *testing.T) {
	boom := errors.New("dial refused")
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) { return fetch.Result{}, boom },
	}
	hooks := directHooks()
	tools, err := marktools.New(backend, hooks)
	if err != nil {
		t.Fatal(err)
	}

	// The prefix appears once, whatever the resolver's own wording.
	if got := tools.Versions(t.Context(), "bad"); !got.IsError || got.Text != "invalid URL: unsupported scheme" {
		t.Errorf("bad URL = %+v", got)
	}
	if len(backend.VersionsCalls) != 0 {
		t.Error("a URL that does not resolve must not reach the backend")
	}
	if got := tools.Versions(t.Context(), "/doc.md"); !got.IsError || got.Text != "versions failed: dial refused" {
		t.Errorf("transport error = %+v", got)
	}

	// A surface with its own error vocabulary, as the broker has, maps it once.
	hooks.ErrText = func(site marktools.Site, host string, err error) string {
		return string(site) + " on " + host + ": " + err.Error()
	}
	tools, err = marktools.New(backend, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.Versions(t.Context(), "/doc.md"); got.Text != "versions on host:6309: dial refused" {
		t.Errorf("mapped error = %+v", got)
	}
}

func TestNewRequiresABackendAndAResolver(t *testing.T) {
	if _, err := marktools.New(nil, directHooks()); err == nil {
		t.Error("nil backend accepted")
	}
	if _, err := marktools.New(&fetchtest.Client{}, marktools.Hooks{}); err == nil {
		t.Error("missing Resolve hook accepted")
	}
}
