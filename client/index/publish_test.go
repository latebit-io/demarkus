package index

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

type memoryIndexRemote struct {
	mu           sync.Mutex
	docs         map[string]protocol.Response
	history      map[string]map[int]protocol.Response
	failManifest bool
	publishes    []string
}

func newMemoryIndexRemote() *memoryIndexRemote {
	return &memoryIndexRemote{docs: make(map[string]protocol.Response), history: make(map[string]map[int]protocol.Response)}
}

func (r *memoryIndexRemote) fetch(docPath string) (protocol.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if base, version, ok := splitTestVersionPath(docPath); ok {
		if doc, exists := r.history[base][version]; exists {
			return doc, nil
		}
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}
	if doc, ok := r.docs[docPath]; ok {
		return doc, nil
	}
	return protocol.Response{Status: protocol.StatusNotFound}, nil
}

func (r *memoryIndexRemote) publish(path, body string, expected int) (protocol.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failManifest && path == "/index.md" {
		return protocol.Response{}, errors.New("simulated manifest conflict")
	}
	current, exists := r.docs[path]
	currentVersion := 0
	if exists {
		currentVersion, _ = strconv.Atoi(current.Metadata["version"])
	}
	if expected != currentVersion {
		return protocol.Response{Status: protocol.StatusConflict}, nil
	}
	version := currentVersion + 1
	status := protocol.StatusOK
	if !exists {
		status = protocol.StatusCreated
	}
	r.docs[path] = protocol.Response{
		Status: protocol.StatusOK, Body: body,
		Metadata: map[string]string{"version": strconv.Itoa(version), "content-hash": BodyHash(body)},
	}
	if r.history[path] == nil {
		r.history[path] = make(map[int]protocol.Response)
	}
	r.history[path][version] = r.docs[path]
	r.publishes = append(r.publishes, path)
	return protocol.Response{Status: status, Metadata: map[string]string{"version": strconv.Itoa(version)}}, nil
}

func splitTestVersionPath(docPath string) (base string, version int, ok bool) {
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

func TestPublishGenerationCreatesAndVerifiesManifestLast(t *testing.T) {
	remote := newMemoryIndexRemote()
	result, err := PublishGeneration(t.Context(), PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}},
	}, remote.fetch, remote.publish)
	if err != nil {
		t.Fatalf("PublishGeneration: %v", err)
	}
	if result.Manifest.ActiveSlot != SlotA || result.ManifestVersion != 1 || result.ShardsPublished != 1 {
		t.Fatalf("result = %+v", result)
	}
	if got := remote.publishes[len(remote.publishes)-1]; got != "/index.md" {
		t.Fatalf("last publish = %q", got)
	}
	if _, err := EntriesForHash("/index.md", remote.docs["/index.md"].Body, testHashA, remote.fetch); err != nil {
		t.Fatalf("published generation is unreadable: %v", err)
	}
}

func TestPublishGenerationAlternatesSlotsAndReusesShard(t *testing.T) {
	remote := newMemoryIndexRemote()
	opts := PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}},
	}
	first, err := PublishGeneration(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	opts.Indexed = opts.Indexed.Add(time.Minute)
	second, err := PublishGeneration(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	opts.Indexed = opts.Indexed.Add(time.Minute)
	third, err := PublishGeneration(t.Context(), opts, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.ActiveSlot != SlotA || second.Manifest.ActiveSlot != SlotB || third.Manifest.ActiveSlot != SlotA {
		t.Fatalf("slots = %s, %s, %s", first.Manifest.ActiveSlot, second.Manifest.ActiveSlot, third.Manifest.ActiveSlot)
	}
	if third.ShardsReused != 1 || third.ShardsPublished != 0 {
		t.Fatalf("third result = %+v", third)
	}
}

func TestPublishGenerationManifestFailurePreservesPriorGeneration(t *testing.T) {
	remote := newMemoryIndexRemote()
	opts := PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}},
	}
	if _, err := PublishGeneration(t.Context(), opts, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}
	prior := remote.docs["/index.md"].Body
	remote.failManifest = true
	opts.Indexed = opts.Indexed.Add(time.Minute)
	opts.Entries = []Entry{{Hash: testHashB, Server: "mark://b", Path: "/b.md"}}
	if _, err := PublishGeneration(t.Context(), opts, remote.fetch, remote.publish); err == nil {
		t.Fatal("manifest failure returned nil")
	}
	if remote.docs["/index.md"].Body != prior {
		t.Fatal("failed generation replaced prior manifest")
	}
	if _, ok := remote.docs["/index.shards/b/ab-000.md"]; !ok {
		t.Fatal("inactive staged shard missing")
	}
}

func TestCommittedManifestPinsImmutableShardVersion(t *testing.T) {
	remote := newMemoryIndexRemote()
	result, err := PublishGeneration(t.Context(), PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/original.md"}},
	}, remote.fetch, remote.publish)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := BuildShards("/index.md", result.Manifest.ActiveSlot, []Entry{
		{Hash: testHashA, Server: "mark://a", Path: "/replacement.md"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ref := result.Manifest.Shards[0]
	if _, err := remote.publish(ref.Path, replacement[0].Body, ref.Version); err != nil {
		t.Fatal(err)
	}
	matches, err := EntriesForHash("/index.md", result.ManifestBody, testHashA, remote.fetch)
	if err != nil {
		t.Fatalf("EntriesForHash: %v", err)
	}
	if len(matches) != 1 || matches[0].Path != "/original.md" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestPublishGenerationHonorsManifestCAS(t *testing.T) {
	remote := newMemoryIndexRemote()
	legacy := Build("legacy", time.Now(), []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}})
	remote.docs["/index.md"] = protocol.Response{
		Status: protocol.StatusOK, Body: legacy,
		Metadata: map[string]string{"version": "4", "content-hash": BodyHash(legacy)},
	}
	remote.history["/index.md"] = map[int]protocol.Response{4: remote.docs["/index.md"]}
	expected := 3
	_, err := PublishGeneration(context.Background(), PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries:                 []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}},
		ExpectedManifestVersion: &expected,
	}, remote.fetch, remote.publish)
	if err == nil || len(remote.publishes) != 0 {
		t.Fatalf("err = %v, publishes = %v", err, remote.publishes)
	}
}

func TestPublishGenerationRejectsBadVerification(t *testing.T) {
	remote := newMemoryIndexRemote()
	publish := func(path, body string, expected int) (protocol.Response, error) {
		resp, err := remote.publish(path, body, expected)
		if path != "/index.md" {
			doc := remote.docs[path]
			doc.Metadata["content-hash"] = fmt.Sprintf("sha256-%064d", 0)
			remote.docs[path] = doc
		}
		return resp, err
	}
	_, err := PublishGeneration(t.Context(), PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/a.md"}},
	}, remote.fetch, publish)
	if err == nil {
		t.Fatal("bad shard verification accepted")
	}
}

func TestConcurrentPublishersCommitOneCompleteGeneration(t *testing.T) {
	remote := newMemoryIndexRemote()
	if _, err := PublishGeneration(t.Context(), PublishOptions{
		ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
		Entries: []Entry{{Hash: testHashA, Server: "mark://a", Path: "/initial.md"}},
	}, remote.fetch, remote.publish); err != nil {
		t.Fatal(err)
	}

	expected := 1
	var successes atomic.Int32
	errs := make(chan error, 2)
	for _, docPath := range []string{"/writer-a.md", "/writer-b.md"} {
		go func() {
			_, err := PublishGeneration(t.Context(), PublishOptions{
				ManifestPath: "/index.md", Source: "aggregated", Indexed: time.Now(),
				Entries:                 []Entry{{Hash: testHashA, Server: "mark://a", Path: docPath}},
				ExpectedManifestVersion: &expected,
			}, remote.fetch, remote.publish)
			if err == nil {
				successes.Add(1)
			}
			errs <- err
		}()
	}
	for range 2 {
		<-errs
	}
	if successes.Load() != 1 {
		t.Fatalf("successful publishers = %d, want 1", successes.Load())
	}
	manifest := remote.docs["/index.md"]
	matches, err := EntriesForHash("/index.md", manifest.Body, testHashA, remote.fetch)
	if err != nil {
		t.Fatalf("committed generation is unreadable: %v", err)
	}
	if len(matches) != 1 || (matches[0].Path != "/writer-a.md" && matches[0].Path != "/writer-b.md") {
		t.Fatalf("committed matches = %+v", matches)
	}
}
