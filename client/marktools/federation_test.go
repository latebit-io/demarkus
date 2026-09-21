package marktools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

const resolveHash = "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

// hostHooks resolve mark://name/path to the host "name:6309", so a test can
// tell the index server from the candidates.
func hostHooks() marktools.Hooks {
	hooks := directHooks()
	hooks.Resolve = func(_ context.Context, raw string) (marktools.Target, error) {
		rest, ok := strings.CutPrefix(raw, "mark://")
		if !ok {
			return marktools.Target{}, errors.New("unsupported scheme")
		}
		name, path, _ := strings.Cut(rest, "/")
		return marktools.Target{Host: name + ":6309", Path: "/" + path}, nil
	}
	return hooks
}

func TestResolveTriesEachCandidateAndVerifiesTheHash(t *testing.T) {
	legacy := index.Build("mark://hub", time.Unix(0, 0), []index.Entry{
		{Hash: resolveHash, Server: "mark://down", Path: "/a.md"},
		{Hash: resolveHash, Server: "mark://liar", Path: "/a.md"},
		{Hash: resolveHash, Server: "mark://good", Path: "/a.md"},
	})
	doc := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# A\n", Metadata: map[string]string{"content-hash": resolveHash}}}
	backend := &fetchtest.Client{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		switch r.Host {
		case "hub:6309":
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: legacy}}, nil
		case "down:6309":
			return fetch.Result{}, errors.New("dial refused")
		case "liar:6309":
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"content-hash": "sha256-other"}}}, nil
		}
		return doc, nil
	}}
	tools := newTools(t, backend, hostHooks())

	got := tools.ResolveHash(t.Context(), marktools.ResolveArgs{Hash: resolveHash, Index: "mark://hub/index.md"})
	if got.IsError || got.Text != mcpfmt.Full(doc, "version", "modified", "content-hash") {
		t.Fatalf("ResolveHash = %+v", got)
	}
	last := backend.FetchCalls[len(backend.FetchCalls)-1]
	if last.Host != "good:6309" || last.Path != "/"+resolveHash || last.Token != "token-for-good:6309" {
		t.Errorf("winning fetch = %+v", last)
	}
}

func TestResolveFailures(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, hostHooks())
	for name, tt := range map[string]struct {
		args marktools.ResolveArgs
		want string
	}{
		"bad hash":      {marktools.ResolveArgs{Hash: "md5-x", Index: "mark://hub/index.md"}, "invalid hash format: expected sha256-<64 lowercase hex characters>"},
		"bad index url": {marktools.ResolveArgs{Hash: resolveHash, Index: "http://hub/index.md"}, "invalid index URL: unsupported scheme"},
		"index missing": {marktools.ResolveArgs{Hash: resolveHash, Index: "mark://hub/index.md"}, "index fetch returned: not-found"},
	} {
		if got := tools.ResolveHash(t.Context(), tt.args); !got.IsError || got.Text != tt.want {
			t.Errorf("%s: got %+v, want %q", name, got, tt.want)
		}
	}
	backend.FetchFn = func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{}, errors.New("dial refused")
	}
	if got := tools.ResolveHash(t.Context(), marktools.ResolveArgs{Hash: resolveHash, Index: "mark://hub/index.md"}); got.Text != "failed to fetch index: dial refused" {
		t.Errorf("unreachable index = %+v", got)
	}
}
