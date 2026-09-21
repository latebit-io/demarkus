package marktools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// indexHooks names servers by identity and writes as "publishing".
func indexHooks(verbs *[]string) marktools.Hooks {
	hooks := hostHooks()
	resolve := hooks.Resolve
	hooks.Resolve = func(ctx context.Context, raw string) (marktools.Target, error) {
		target, err := resolve(ctx, raw)
		target.Authority = "mark://" + strings.TrimSuffix(target.Host, ":6309")
		return target, err
	}
	hooks.Agent = func(context.Context) string { return "test-agent" }
	hooks.Now = func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }
	hooks.Writer = func(_ context.Context, _ marktools.Target, verb string) (marktools.WriteFunc, error) {
		*verbs = append(*verbs, verb)
		return docwrite.SendOnce("write-token"), nil
	}
	return hooks
}

func sourceBackend() *fetchtest.Client {
	const hash = "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	return &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			switch {
			case r.Path == protocol.WellKnownManifestPath:
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Agent Manifest\n"}}, nil
			case r.Host == "hub:6309":
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Doc", Metadata: map[string]string{"content-hash": hash}}}, nil
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage(r.Path, "", "doc.md"), nil
		},
	}
}

func TestIndexPublishesAGeneration(t *testing.T) {
	var verbs []string
	backend := sourceBackend()
	tools := newTools(t, backend, indexHooks(&verbs))

	got := tools.Index(t.Context(), marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/indexes/source.md"})
	want := "Indexed 1 documents from mark://source\nstatus: ok\nversion: 1\nshards-published: 1\nshards-reused: 0\n"
	if got.IsError || got.Text != want {
		t.Fatalf("Index = %q\nwant    %q", got.Text, want)
	}
	if len(backend.PublishCalls) != 2 || backend.PublishCalls[1].Path != "/indexes/source.md" {
		t.Fatalf("publishes = %+v, want a shard then the manifest", backend.PublishCalls)
	}
	manifest := backend.PublishCalls[1]
	if manifest.Token != "write-token" || manifest.Metadata["agent"] != "test-agent" ||
		!strings.Contains(manifest.Body, "> Source: mark://source\n") || !strings.Contains(manifest.Body, "> Indexed: 2026-09-21T12:00:00Z\n") {
		t.Errorf("manifest publish = %+v", manifest)
	}
	if len(verbs) != 1 || verbs[0] != "publishing" {
		t.Errorf("writer verbs = %v, want one authorization, worded as publishing", verbs)
	}
	// A server may require a token for every read, its manifest included.
	for _, r := range backend.FetchCalls {
		if r.Path == protocol.WellKnownManifestPath && r.Token != "token-for-"+r.Host {
			t.Errorf("manifest check of %s sent token %q", r.Host, r.Token)
		}
	}
}

func TestIndexDryRunPublishesNothingAndNeedsNoWriter(t *testing.T) {
	var verbs []string
	backend := sourceBackend()
	tools := newTools(t, backend, indexHooks(&verbs))
	got := tools.Index(t.Context(), marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/indexes/source.md", DryRun: true})
	if got.IsError || !strings.HasPrefix(got.Text, "Indexed 1 documents from mark://source (dry run, logical entry preview only; publication writes a v2 manifest and shards)\n\n") ||
		!strings.Contains(got.Text, "| mark://source | /doc.md |") {
		t.Errorf("dry run = %q", got.Text)
	}
	if len(backend.PublishCalls) != 0 || len(verbs) != 0 {
		t.Errorf("publishes = %d, writer asked %v", len(backend.PublishCalls), verbs)
	}
}

func TestIndexRefusals(t *testing.T) {
	var verbs []string
	tools := newTools(t, sourceBackend(), indexHooks(&verbs))
	for name, tt := range map[string]struct {
		args marktools.IndexArgs
		want string
	}{
		"bad source": {marktools.IndexArgs{Source: "http://x", Target: "mark://hub/i.md"}, "invalid source URL: unsupported scheme"},
		"bad target": {marktools.IndexArgs{Source: "mark://source", Target: "http://x"}, "invalid target URL: unsupported scheme"},
		"negative":   {marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/i.md", ExpectedVersion: -1}, "expected_version must be non-negative"},
	} {
		if got := tools.Index(t.Context(), tt.args); !got.IsError || got.Text != tt.want {
			t.Errorf("%s: got %+v, want %q", name, got, tt.want)
		}
	}
}

// A caller who may not publish is refused before the crawl, not after it.
func TestIndexAuthorizesBeforeAnyNetworkWork(t *testing.T) {
	backend := sourceBackend()
	var verbs []string
	hooks := indexHooks(&verbs)
	hooks.Writer = func(context.Context, marktools.Target, string) (marktools.WriteFunc, error) {
		return nil, errors.New("publishing requires a token")
	}
	got := newTools(t, backend, hooks).Index(t.Context(), marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/i.md"})
	if !got.IsError || got.Text != "publishing requires a token" {
		t.Errorf("Index = %+v", got)
	}
	if n := len(backend.FetchCalls) + len(backend.ListCalls) + len(backend.PublishCalls); n != 0 {
		t.Errorf("backend calls = %d, want none for a refused caller", n)
	}
}

// manifestBackend is sourceBackend with one server's manifest answer replaced.
func manifestBackend(host string, answer func() (fetch.Result, error)) *fetchtest.Client {
	backend := sourceBackend()
	inner := backend.FetchFn
	backend.FetchFn = func(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		if r.Host == host && r.Path == protocol.WellKnownManifestPath {
			return answer()
		}
		return inner(ctx, r)
	}
	return backend
}

// A manifest that could not be read is not a missing manifest: force
// overrides the second, never the first.
func TestIndexManifestTransportFailure(t *testing.T) {
	unreachable := func() (fetch.Result, error) { return fetch.Result{}, errors.New("dial timeout") }
	forced := marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/i.md", Force: true}
	var verbs []string

	source := newTools(t, manifestBackend("source:6309", unreachable), indexHooks(&verbs))
	if got := source.Index(t.Context(), forced); got.IsError || !strings.HasPrefix(got.Text, "warning: could not check source agent manifest: dial timeout\n") {
		t.Errorf("source unreachable = %+v, want a warning with the cause and the run to go on", got)
	}
	target := newTools(t, manifestBackend("hub:6309", unreachable), indexHooks(&verbs))
	if got := target.Index(t.Context(), forced); !got.IsError || got.Text != "could not check target agent manifest: dial timeout" {
		t.Errorf("target unreachable = %+v, want a block despite force", got)
	}
}

// force answers a missing target manifest only; any other status blocks.
func TestIndexForceOnlyOverridesAMissingManifest(t *testing.T) {
	for status, wantBlock := range map[string]bool{
		protocol.StatusNotFound:     false,
		protocol.StatusUnauthorized: true,
		protocol.StatusNotPermitted: true,
		protocol.StatusServerError:  true,
	} {
		var verbs []string
		backend := manifestBackend("hub:6309", func() (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: status}}, nil
		})
		tools := newTools(t, backend, indexHooks(&verbs))
		got := tools.Index(t.Context(), marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/i.md", Force: true})
		if got.IsError != wantBlock {
			t.Errorf("%s: %+v, want blocked=%v", status, got, wantBlock)
		}
		if wantBlock && got.Text != "could not check target agent manifest: status "+status {
			t.Errorf("%s: block text = %q", status, got.Text)
		}
		if !wantBlock && !strings.HasPrefix(got.Text, "warning: target server has no agent manifest (force=true override)\n") {
			t.Errorf("%s: forced run = %q", status, got.Text)
		}
	}
	unforced := newTools(t, manifestBackend("hub:6309", func() (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}), indexHooks(new([]string)))
	if got := unforced.Index(t.Context(), marktools.IndexArgs{Source: "mark://source", Target: "mark://hub/i.md"}); !got.IsError || !strings.Contains(got.Text, "Use force=true to override") {
		t.Errorf("unforced = %+v", got)
	}
}
