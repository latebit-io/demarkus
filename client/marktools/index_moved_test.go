package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// mvListRoot lists one document at the root and nothing below it.
func mvListRoot(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
	if r.Path == "/" {
		return fetchtest.ListPage("/", "", "a.md"), nil
	}
	return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
}

func mvCreated(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
	return fetch.Result{Response: protocol.Response{Status: "created", Metadata: map[string]string{"version": "1"}}}, nil
}

func mvIndexArgs() marktools.IndexArgs {
	return marktools.IndexArgs{Source: "mark://source.com", Target: "mark://hub.com/indexes/source.md"}
}

func TestIndexDryRun(t *testing.T) {
	hash := "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Manifest"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "content",
			}}, nil
		},
		ListFn: mvListRoot,
		PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
			t.Fatal("publish should not be called in dry_run mode")
			return fetch.Result{}, nil
		},
	}
	args := mvIndexArgs()
	args.DryRun = true
	got := (&clientSurface{}).tools(t, backend).Index(t.Context(), args)
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	if !strings.Contains(got.Text, "dry run") {
		t.Error("dry run output should mention dry run")
	}
	if !strings.Contains(got.Text, hash) {
		t.Error("dry run output should contain the index content")
	}
	if !strings.Contains(got.Text, "| mark://source.com |") || strings.Contains(got.Text, ":6309") {
		t.Errorf("dry run should name the server by identity:\n%s", got.Text)
	}
}

func TestIndexBlocksWithoutManifest(t *testing.T) {
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			if r.Host == "hub.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
		},
		ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage("/", "", "a.md"), nil
		},
	}
	tools := (&clientSurface{DefaultHost: "mark://example.com", Token: "test-token"}).tools(t, backend)
	mvAssertError(t, tools.Index(t.Context(), mvIndexArgs()), "no agent manifest")
}

func TestIndexForceOverridesManifest(t *testing.T) {
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if r.Path == protocol.WellKnownManifestPath {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			if r.Host == "hub.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				Body:     "content",
			}}, nil
		},
		ListFn:    mvListRoot,
		PublishFn: mvCreated,
	}
	tools := (&clientSurface{DefaultHost: "mark://hub.com", Token: "test-token"}).tools(t, backend)
	args := mvIndexArgs()
	args.Force = true
	got := tools.Index(t.Context(), args)
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	if !strings.Contains(got.Text, "warning") {
		t.Error("should include warning about missing manifest")
	}
}

func TestIndexSourceNoManifestWarns(t *testing.T) {
	hash := "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			host, path := r.Host, r.Path
			if path == protocol.WellKnownManifestPath {
				if host == "source.com:6309" {
					return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
				}
				// Target has manifest.
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Hub"}}, nil
			}
			if host == "hub.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": hash},
				Body:     "content",
			}}, nil
		},
		ListFn:    mvListRoot,
		PublishFn: mvCreated,
	}
	tools := (&clientSurface{DefaultHost: "mark://hub.com", Token: "test-token"}).tools(t, backend)
	got := tools.Index(t.Context(), mvIndexArgs())
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	if !strings.Contains(got.Text, "warning: source") {
		t.Error("should warn about source having no manifest")
	}
}
