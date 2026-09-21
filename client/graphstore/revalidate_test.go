package graphstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

func TestRevalidationSingleFlightAndCancellation(t *testing.T) {
	store := New()
	g := revisionGraph(3, "last-good")
	node := g.GetNode(freshnessSource)
	node.Observation.ObservedAt = time.Now().Add(-time.Hour)
	node.Observation.AttemptedAt = node.Observation.ObservedAt
	store.Merge(g, nil)
	started := make(chan struct{})
	var calls atomic.Int32
	fetchFn := func(ctx context.Context, _ links.Target) (graph.FetchResult, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return graph.FetchResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.Revalidate(ctx, nil, fetchFn); done <- err }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("revalidation finished before fetching: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("revalidation did not start")
	}
	second, err := store.Revalidate(t.Context(), nil, fetchFn)
	if err != nil || second.Attempts != 0 {
		t.Fatalf("duplicate revalidation: %+v, %v", second, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revalidation did not stop after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d", calls.Load())
	}
	requireTargets(t, store, "last-good")
	if store.GetNode(freshnessSource).Observation.Freshness() != "stale" {
		t.Fatal("cancelled source was marked fresh")
	}
}

func TestRevalidationBoundsPreFetchFailures(t *testing.T) {
	store := New()
	for i := range RevalidationLimit + 1 {
		store.ReplaceSeed(fmt.Sprint(i), []StoredNode{{URL: fmt.Sprintf("mark://source/%d.md", i), Status: "ok"}}, nil)
	}
	result, err := store.Revalidate(t.Context(), nil, nil)
	if err == nil || result.Attempts != RevalidationLimit || result.Remaining != 1 || result.Fetches != 0 {
		t.Fatalf("unbounded pre-fetch failures: %+v, %v", result, err)
	}
}

func TestRevalidationByteCapPreservesLastGood(t *testing.T) {
	store := New()
	g := revisionGraph(3, "last-good")
	g.GetNode(freshnessSource).Observation.AttemptedAt = time.Time{}
	store.Merge(g, nil)
	result, err := store.Revalidate(t.Context(), nil, func(context.Context, links.Target) (graph.FetchResult, error) {
		return graph.FetchResult{Status: "ok", Body: strings.Repeat("x", int(revalidationBytes)+1), Metadata: map[string]string{"version": "4"}}, nil
	})
	if !errors.Is(err, graph.ErrIncomplete) || result.Fetches != 1 {
		t.Fatalf("byte cap: %+v, %v", result, err)
	}
	requireTargets(t, store, "last-good")
	if store.GetNode(freshnessSource).Observation.Revision != 3 {
		t.Fatal("capped source replaced accepted revision")
	}
}

func TestSnapshotV1RemainsUnknown(t *testing.T) {
	artifacts, err := BuildSnapshotShards(SnapshotManifestPath, generation.SlotA, []StoredNode{{URL: freshnessSource, Status: "ok"}}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	artifact := artifacts[0]
	artifact.Body = strings.ReplaceAll(artifact.Body, SnapshotShardFormat, "demarkus-graph-snapshot-shard/v1")
	artifact.ContentHash = generation.BodyHash(artifact.Body)
	artifact.Bytes = len(artifact.Body)
	manifest, err := BuildSnapshotManifest(SnapshotManifestPath, SnapshotManifest{Exported: time.Now(), Complete: true, Nodes: 1, ActiveSlot: generation.SlotA, Shards: []SnapshotShardRef{artifact.Ref(83)}})
	if err != nil {
		t.Fatal(err)
	}
	manifest = strings.ReplaceAll(manifest, SnapshotManifestFormat, "demarkus-graph-snapshot/v1")
	nodes, _, err := LoadSnapshot(SnapshotManifestPath, protocol.Response{Status: "ok", Body: manifest, Metadata: map[string]string{"content-hash": generation.BodyHash(manifest)}}, func(string) (protocol.Response, error) {
		return protocol.Response{Status: "ok", Body: artifact.Body, Metadata: map[string]string{"version": "83", "content-hash": artifact.ContentHash}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].Observation.Freshness() != "unknown" || nodes[0].Observation.Revision != 0 {
		t.Fatalf("v1 inferred freshness from shard revision: %+v", nodes[0])
	}
}

func TestSameRevisionArchiveUnarchiveAndEqualRefresh(t *testing.T) {
	store := New()
	initial := revisionGraph(3, "target")
	initial.GetNode(freshnessSource).Observation.ObservedAt = time.Now().Add(-time.Hour)
	store.Merge(initial, nil)
	seedRevision(store, "hub", revisionGraph(3, "target"))
	if store.GetNode(freshnessSource).Observation.Freshness() != "fresh" {
		t.Fatal("equal matching observation did not refresh age")
	}
	archived := revisionGraph(3, "")
	archived.GetNode(freshnessSource).Status = "archived"
	store.Merge(archived, nil)
	requireTargets(t, store)
	if store.GetNode(freshnessSource).Observation.Freshness() != "fresh" {
		t.Fatal("old seed invalidated confirmed archive state")
	}
	store.Merge(revisionGraph(3, "target"), nil)
	requireTargets(t, store, "target")
}
