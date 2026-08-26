package graphstore

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func TestGraphSnapshotRoundTripAndIntegrity(t *testing.T) {
	exported := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	nodes := []StoredNode{
		{URL: "mark://b/b.md", Title: "B", Status: "ok", LinkCount: 1},
		{URL: "mark://a/a.md", Title: "A", Status: "ok", LinkCount: 1},
	}
	edges := []StoredEdge{
		{From: "mark://b/b.md", To: "mark://a/a.md", Count: 1},
		{From: "mark://a/a.md", To: "mark://b/b.md", Rel: "related", Label: "B", Count: 2},
	}
	one, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA, nodes[:1], nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	oneEdge, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA, nil, edges[1:], 0)
	if err != nil {
		t.Fatal(err)
	}
	target := max(one[0].Bytes, oneEdge[0].Bytes)
	artifacts, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA, nodes, edges, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) < 2 {
		t.Fatalf("shards = %d, want separate node and edge shards", len(artifacts))
	}
	refs := make([]SnapshotShardRef, len(artifacts))
	responses := make(map[string]protocol.Response, len(artifacts))
	for i, artifact := range artifacts {
		refs[i] = artifact.Ref(i + 1)
		responses[SnapshotVersionPath(artifact.Path, i+1)] = protocol.Response{
			Status: protocol.StatusOK, Body: artifact.Body,
			Metadata: map[string]string{"version": strconv.Itoa(i + 1), "content-hash": artifact.ContentHash},
		}
	}
	manifestBody, err := BuildSnapshotManifest(SnapshotManifestPath, SnapshotManifest{
		Exported: exported, Complete: true, Nodes: len(nodes), Edges: len(edges),
		ActiveSlot: SnapshotSlotA, Shards: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestResponse := protocol.Response{Status: protocol.StatusOK, Body: manifestBody, Metadata: map[string]string{
		"content-hash": SnapshotBodyHash(manifestBody),
	}}
	loadedNodes, loadedEdges, err := LoadSnapshot(SnapshotManifestPath, manifestResponse, func(path string) (protocol.Response, error) {
		return responses[path], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedNodes) != 2 || loadedNodes[0].URL != "mark://a/a.md" || len(loadedEdges) != 2 || loadedEdges[0].From != "mark://a/a.md" {
		t.Fatalf("loaded nodes=%+v edges=%+v", loadedNodes, loadedEdges)
	}

	corruptPath := SnapshotVersionPath(refs[0].Path, refs[0].Version)
	missingVersion := responses[corruptPath]
	missingVersion.Metadata = maps.Clone(missingVersion.Metadata)
	delete(missingVersion.Metadata, "version")
	if _, _, err := verifySnapshotShard(refs[0], missingVersion); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("missing version error = %v", err)
	}
	corrupt := responses[corruptPath]
	corrupt.Body += "x"
	responses[corruptPath] = corrupt
	if _, _, err := LoadSnapshot(SnapshotManifestPath, manifestResponse, func(path string) (protocol.Response, error) {
		return responses[path], nil
	}); err == nil || !strings.Contains(err.Error(), "content mismatch") {
		t.Fatalf("corrupt snapshot error = %v", err)
	}
}

func TestGraphSnapshotRejectsAliasesAndMalformedGeneratedContent(t *testing.T) {
	if _, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA, []StoredNode{
		{URL: "mark://host/a.md"},
		{URL: "mark://host:6309/a.md"},
	}, nil, 0); err == nil || !strings.Contains(err.Error(), "duplicate graph node") {
		t.Fatalf("alias error = %v", err)
	}
	manifest, err := BuildSnapshotManifest(SnapshotManifestPath, SnapshotManifest{
		Exported: time.Now(), Complete: true, ActiveSlot: SnapshotSlotA,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest = strings.Replace(manifest, "\n\n| Kind", "\nunexpected\n\n| Kind", 1)
	if _, err := ParseSnapshotManifest(SnapshotManifestPath, manifest); err == nil || !strings.Contains(err.Error(), "unexpected snapshot content") {
		t.Fatalf("preamble error = %v", err)
	}
	if err := validateSnapshotJSON(`{"nodes":[],"nodes":[]}`); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate JSON error = %v", err)
	}
}

func TestGraphSnapshotRejectsUnrepresentableManifestValues(t *testing.T) {
	if got := SnapshotShardRoot(SnapshotManifestPath); got != "/graph/shards" {
		t.Fatalf("SnapshotShardRoot = %q", got)
	}
	if _, err := BuildSnapshotManifest("/graph/man|ifest.md", SnapshotManifest{
		Exported: time.Now(), Complete: true, ActiveSlot: SnapshotSlotA,
	}); err == nil {
		t.Fatal("manifest path containing table delimiter accepted")
	}
	for _, manifestPath := range []string{"/graph/v2", "/graph/manifest", "/sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if _, err := BuildSnapshotManifest(manifestPath, SnapshotManifest{
			Exported: time.Now(), Complete: true, ActiveSlot: SnapshotSlotA,
		}); err == nil {
			t.Fatalf("special fetch path %q accepted", manifestPath)
		}
	}
	if _, err := BuildSnapshotManifest(SnapshotManifestPath, SnapshotManifest{
		Exported: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), Complete: true, ActiveSlot: SnapshotSlotA,
	}); err == nil {
		t.Fatal("year zero accepted")
	}
	artifacts, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA,
		[]StoredNode{{URL: "mark://a/a.md", Status: "ok"}}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ref := artifacts[0].Ref(1)
	ref.Bytes = protocol.MaxBodyLength + 1
	if _, err := BuildSnapshotManifest(SnapshotManifestPath, SnapshotManifest{
		Exported: time.Now(), Complete: true, Nodes: 1, ActiveSlot: SnapshotSlotA, Shards: []SnapshotShardRef{ref},
	}); err == nil {
		t.Fatal("oversized shard descriptor accepted")
	}
}

type snapshotRemote struct {
	mu        sync.Mutex
	docs      map[string]protocol.Response
	history   map[string]map[int]protocol.Response
	publishes []string
	failRoot  bool
}

func newSnapshotRemote() *snapshotRemote {
	return &snapshotRemote{docs: make(map[string]protocol.Response), history: make(map[string]map[int]protocol.Response)}
}

func (r *snapshotRemote) fetch(_ context.Context, docPath string) (protocol.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if base, version, ok := splitSnapshotVersionPath(docPath); ok {
		if response, exists := r.history[base][version]; exists {
			return response, nil
		}
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}
	if response, exists := r.docs[docPath]; exists {
		return response, nil
	}
	return protocol.Response{Status: protocol.StatusNotFound}, nil
}

func (r *snapshotRemote) publish(_ context.Context, docPath, body string, expectedVersion int) (protocol.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRoot && docPath == SnapshotManifestPath {
		return protocol.Response{Status: protocol.StatusConflict}, nil
	}
	currentVersion := 0
	if current, exists := r.docs[docPath]; exists {
		var err error
		currentVersion, err = strconv.Atoi(current.Metadata["version"])
		if err != nil {
			return protocol.Response{}, err
		}
	}
	if expectedVersion != currentVersion {
		return protocol.Response{Status: protocol.StatusConflict}, nil
	}
	version := currentVersion + 1
	stored := protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{
		"version": strconv.Itoa(version), "content-hash": SnapshotBodyHash(body),
	}}
	r.docs[docPath] = stored
	if r.history[docPath] == nil {
		r.history[docPath] = make(map[int]protocol.Response)
	}
	historical := stored
	historical.Metadata = maps.Clone(stored.Metadata)
	r.history[docPath][version] = historical
	r.publishes = append(r.publishes, docPath)
	status := protocol.StatusOK
	if currentVersion == 0 {
		status = protocol.StatusCreated
	}
	return protocol.Response{Status: status, Metadata: map[string]string{"version": strconv.Itoa(version)}}, nil
}

func TestPublishGraphSnapshotCommitsManifestLast(t *testing.T) {
	remote := newSnapshotRemote()
	result, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
		Nodes: []StoredNode{{URL: "mark://a/a.md", Status: "ok"}},
		Edges: []StoredEdge{{From: "mark://a/a.md", To: "mark://b/b.md", Count: 1}},
	}, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestVersion != 1 || result.Manifest.ActiveSlot != SnapshotSlotA || result.ShardsPublished != 2 {
		t.Fatalf("result = %+v", result)
	}
	if got := remote.publishes[len(remote.publishes)-1]; got != SnapshotManifestPath {
		t.Fatalf("last publish = %q", got)
	}
	manifestResponse := remote.docs[SnapshotManifestPath]
	if _, _, err := LoadSnapshot(SnapshotManifestPath, manifestResponse, func(path string) (protocol.Response, error) {
		return remote.fetch(t.Context(), path)
	}); err != nil {
		t.Fatalf("committed snapshot unreadable: %v", err)
	}
}

func TestPublishGraphSnapshotFailurePreservesPreviousGeneration(t *testing.T) {
	remote := newSnapshotRemote()
	opts := SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
		Nodes: []StoredNode{{URL: "mark://a/a.md", Title: "A", Status: "ok"}},
	}
	if _, err := PublishSnapshot(t.Context(), opts, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}
	previous := remote.docs[SnapshotManifestPath].Body
	remote.failRoot = true
	opts.Exported = opts.Exported.Add(time.Minute)
	opts.Nodes[0].Title = "Changed"
	if _, err := PublishSnapshot(t.Context(), opts, remote.fetch, remote.publish); err == nil {
		t.Fatal("manifest failure returned nil")
	}
	if remote.docs[SnapshotManifestPath].Body != previous {
		t.Fatal("failed publication replaced committed manifest")
	}
	if _, ok := remote.docs["/graph/shards/b/nodes-000.md"]; !ok {
		t.Fatal("inactive staged shard missing")
	}
}

func TestPublishGraphSnapshotAlternatesAndReusesSlots(t *testing.T) {
	remote := newSnapshotRemote()
	opts := SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
		Nodes: []StoredNode{{URL: "mark://a/a.md", Status: "ok"}},
	}
	first, err := PublishSnapshot(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	opts.Exported = opts.Exported.Add(time.Minute)
	second, err := PublishSnapshot(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	opts.Exported = opts.Exported.Add(time.Minute)
	third, err := PublishSnapshot(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.ActiveSlot != SnapshotSlotA || second.Manifest.ActiveSlot != SnapshotSlotB || third.Manifest.ActiveSlot != SnapshotSlotA {
		t.Fatalf("slots = %s, %s, %s", first.Manifest.ActiveSlot, second.Manifest.ActiveSlot, third.Manifest.ActiveSlot)
	}
	if third.ShardsReused != 1 || third.ShardsPublished != 0 {
		t.Fatalf("third publication = %+v", third)
	}
}

func TestGraphSnapshotPinsImmutableShardVersion(t *testing.T) {
	remote := newSnapshotRemote()
	result, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
		Nodes: []StoredNode{{URL: "mark://a/a.md", Title: "Original", Status: "ok"}},
	}, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	ref := result.Manifest.Shards[0]
	replacement, err := BuildSnapshotShards(SnapshotManifestPath, result.Manifest.ActiveSlot, []StoredNode{{URL: "mark://a/a.md", Title: "Replacement", Status: "ok"}}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.publish(t.Context(), ref.Path, replacement[0].Body, ref.Version); err != nil {
		t.Fatal(err)
	}
	manifest := remote.docs[SnapshotManifestPath]
	nodes, _, err := LoadSnapshot(SnapshotManifestPath, manifest, func(path string) (protocol.Response, error) {
		return remote.fetch(t.Context(), path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Title != "Original" {
		t.Fatalf("loaded nodes = %+v", nodes)
	}
}

func TestGraphSnapshotManifestCASAndCancellation(t *testing.T) {
	remote := newSnapshotRemote()
	if _, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
	}, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}
	publishes := len(remote.publishes)
	stale := 0
	if _, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(), ExpectedManifestVersion: &stale,
	}, remote.fetch, remote.publish); err == nil || len(remote.publishes) != publishes {
		t.Fatalf("stale CAS err=%v publishes=%v", err, remote.publishes)
	}

	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := PublishSnapshot(ctx, SnapshotPublishOptions{ManifestPath: SnapshotManifestPath, Exported: time.Now()},
			func(fetchCtx context.Context, _ string) (protocol.Response, error) {
				close(started)
				<-fetchCtx.Done()
				return protocol.Response{}, fetchCtx.Err()
			}, remote.publish)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestConcurrentGraphSnapshotPublishersCommitOneGeneration(t *testing.T) {
	remote := newSnapshotRemote()
	if _, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
		ManifestPath: SnapshotManifestPath, Exported: time.Now(),
	}, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}
	expected := 1
	var successes atomic.Int32
	done := make(chan struct{}, 2)
	for _, title := range []string{"A", "B"} {
		go func() {
			_, err := PublishSnapshot(t.Context(), SnapshotPublishOptions{
				ManifestPath: SnapshotManifestPath, Exported: time.Now(), ExpectedManifestVersion: &expected,
				Nodes: []StoredNode{{URL: "mark://a/a.md", Title: title, Status: "ok"}},
			}, remote.fetch, remote.publish)
			if err == nil {
				successes.Add(1)
			}
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	if successes.Load() != 1 {
		t.Fatalf("successful publishers = %d, want 1", successes.Load())
	}
}

func splitSnapshotVersionPath(docPath string) (base string, version int, ok bool) {
	cut := strings.LastIndex(docPath, "/v")
	if cut < 1 {
		return "", 0, false
	}
	version, err := strconv.Atoi(docPath[cut+2:])
	if err != nil || version < 1 {
		return "", 0, false
	}
	return docPath[:cut], version, true
}
