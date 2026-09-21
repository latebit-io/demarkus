package marktools_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

const mvHash = "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

func mvResolveArgs() marktools.ResolveArgs {
	return marktools.ResolveArgs{Hash: mvHash, Index: "mark://hub.example.com/index.md"}
}

// A v2 manifest fetches only the shard whose prefix matches the hash.
func TestResolveSharedManifest(t *testing.T) {
	otherHash := "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	artifacts, err := index.BuildShards("/index.md", generation.SlotA, []index.Entry{
		{Hash: mvHash, Server: "mark://docs.example.com", Path: "/guide.md"},
		{Hash: otherHash, Server: "mark://other.example.com", Path: "/other.md"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]index.ShardRef, len(artifacts))
	shards := make(map[string]fetch.Result)
	for i, artifact := range artifacts {
		version := i + 1
		refs[i] = artifact.Ref(version)
		shards[generation.VersionPath(artifact.Path, version)] = fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK, Body: artifact.Body,
			Metadata: map[string]string{"version": strconv.Itoa(version), "content-hash": artifact.ContentHash},
		}}
	}
	manifest, err := index.BuildManifest("/index.md", index.Manifest{
		Source: "aggregated", Indexed: time.Now(), Complete: true,
		Documents: 2, ActiveSlot: generation.SlotA, Shards: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	shardFetches := 0
	backend := &fetchtest.Client{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		path := r.Path
		if path == "/index.md" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: manifest}}, nil
		}
		if shard, ok := shards[path]; ok {
			shardFetches++
			return shard, nil
		}
		if path == "/"+mvHash {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK, Body: "# Guide",
				Metadata: map[string]string{"content-hash": mvHash, "version": "1"},
			}}, nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}}
	got := (&clientSurface{}).tools(t, backend).ResolveHash(t.Context(), mvResolveArgs())
	if got.IsError {
		t.Fatalf("ResolveHash = %+v", got)
	}
	if shardFetches != 1 {
		t.Fatalf("shard fetches = %d, want 1", shardFetches)
	}
}

func TestResolveFallback(t *testing.T) {
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n" +
		"| " + mvHash + " | mark://server1.com | /a.md |\n" +
		"| " + mvHash + " | mark://server2.com | /b.md |\n"

	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if strings.Contains(r.Path, "index") {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: indexBody}}, nil
			}
			// First server fails, second succeeds.
			if r.Host == "server1.com:6309" {
				return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"content-hash": mvHash},
				Body:     "# Found",
			}}, nil
		},
	}
	got := (&clientSurface{}).tools(t, backend).ResolveHash(t.Context(), mvResolveArgs())
	if got.IsError {
		t.Fatalf("unexpected tool error: %v", got.Text)
	}
	if !strings.Contains(got.Text, "# Found") {
		t.Errorf("should contain document from second server, got: %s", got.Text)
	}
}

func TestResolveNotFound(t *testing.T) {
	indexBody := "| Hash | Server | Path |\n|------|--------|------|\n| " + mvHash + " | mark://server.com | /a.md |\n"

	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if strings.Contains(r.Path, "index") {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: indexBody}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: "not-found"}}, nil
		},
	}
	got := (&clientSurface{}).tools(t, backend).ResolveHash(t.Context(), mvResolveArgs())
	mvAssertError(t, got, "could not resolve hash")
}

func TestResolveInvalidHash(t *testing.T) {
	got := (&clientSurface{}).tools(t, &fetchtest.Client{}).ResolveHash(t.Context(),
		marktools.ResolveArgs{Hash: "not-a-hash", Index: "mark://hub.example.com/index.md"})
	mvAssertError(t, got, "invalid hash format")
}
