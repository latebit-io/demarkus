package graphstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// testHub is a versioned hub serving world checkpoints, counting reads.
type testHub struct {
	mu    sync.Mutex
	docs  map[string]protocol.Response
	heads map[string]int
	reads map[string]int
}

func newTestHub() *testHub {
	return &testHub{docs: map[string]protocol.Response{}, heads: map[string]int{}, reads: map[string]int{}}
}

// put writes body to docPath, as a new version only when it changed.
func (h *testHub) put(docPath, body string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if head := h.heads[docPath]; head > 0 && h.docs[docPath].Body == body {
		return head
	}
	h.heads[docPath]++
	version := h.heads[docPath]
	resp := published(body, version)
	resp.Metadata["etag"] = resp.Metadata["content-hash"]
	h.docs[docPath], h.docs[protocol.VersionPath(docPath, version)] = resp, resp
	return version
}

func (h *testHub) remove(docPath string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.docs, docPath)
}

func (h *testHub) fetch(_ context.Context, docPath, ifNoneMatch string) (protocol.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads[docPath]++
	resp, ok := h.docs[docPath]
	switch {
	case !ok:
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	case ifNoneMatch != "" && ifNoneMatch == resp.Metadata["etag"]:
		return protocol.Response{Status: protocol.StatusNotModified}, nil
	}
	return resp, nil
}

// shardReads counts reads of world's pinned shard versions.
func (h *testHub) shardReads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for p, count := range h.reads {
		if p != WorldManifestPath("alpha") && IsGeneratedGraphPath(p) {
			n += count
		}
	}
	return n
}

// checkpoint publishes sources as alpha's checkpoint at prefix length 1.
func (h *testHub) checkpoint(t *testing.T, complete bool, sources ...WorldSource) {
	t.Helper()
	h.publishCheckpoint(t, WorldManifest{World: "alpha", Cursor: protocol.Cursor{Epoch: "e1", Seq: 1}, Complete: complete, PrefixLength: 1}, sources)
}

// checkpointAt publishes a complete checkpoint of sources at cursor.
func (h *testHub) checkpointAt(t *testing.T, cursor protocol.Cursor, sources ...WorldSource) {
	t.Helper()
	h.publishCheckpoint(t, WorldManifest{World: "alpha", Cursor: cursor, Complete: true, PrefixLength: 1}, sources)
}

func (h *testHub) publishCheckpoint(t *testing.T, m WorldManifest, sources []WorldSource) { //nolint:gocritic // a test manifest is built once
	t.Helper()
	byPrefix := map[string][]WorldSource{}
	for _, src := range sources {
		prefix := SourcePrefix(src.Path, 1)
		byPrefix[prefix] = append(byPrefix[prefix], src)
	}
	for prefix, group := range byPrefix {
		shard, err := BuildWorldShard("alpha", prefix, group)
		if err != nil {
			t.Fatal(err)
		}
		m.Shards = append(m.Shards, shard.Ref(h.put(shard.Path, shard.Body)))
	}
	body, err := BuildWorldManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	h.put(WorldManifestPath("alpha"), body)
}

// Paths under distinct prefixes, so each edit touches its own shard.
var (
	seedA = WorldSource{Path: "/a.md", Version: 1, Title: "A", Edges: []WorldEdge{{To: "mark://alpha/b.md", Count: 2}}}
	seedB = WorldSource{Path: "/b.md", Version: 1, Title: "B"}
)

func seedAlpha(ctx context.Context, store *Store, hub *testHub) {
	store.ExpireSeedCheck("alpha")
	store.Seed(ctx, SeedSource{
		Owner: "alpha",
		Fetch: func(context.Context, string, string) (protocol.Response, error) {
			return protocol.Response{Status: protocol.StatusNotFound}, nil
		},
		Hub: hub.fetch,
	})
}

func TestSeedReadsTheWorldCheckpointFromTheHub(t *testing.T) {
	if SourcePrefix(seedA.Path, 1) == SourcePrefix(seedB.Path, 1) {
		t.Fatal("fixture paths share a shard")
	}
	ctx, store, hub := context.Background(), New(), newTestHub()
	hub.checkpoint(t, true, seedA, seedB)
	seedAlpha(ctx, store, hub)
	a := store.GetNode("mark://alpha/a.md")
	if a == nil || a.Title != "A" || a.LinkCount != 2 || !a.Observation.Known() || a.Observation.Revision != 1 {
		t.Fatalf("node a = %+v", a)
	}
	if got := store.Backlinks("mark://alpha/b.md"); len(got) != 1 || got[0] != "mark://alpha/a.md" {
		t.Errorf("backlinks of b = %v", got)
	}

	// One edit refetches only its own shard; the other shard's rows stay.
	reads := hub.shardReads()
	edited := seedA
	edited.Version, edited.Title = 2, "A2"
	hub.checkpoint(t, true, edited, seedB)
	seedAlpha(ctx, store, hub)
	if got := hub.shardReads() - reads; got != 1 {
		t.Errorf("an edit read %d shards, want 1", got)
	}
	if a := store.GetNode("mark://alpha/a.md"); a == nil || a.Title != "A2" {
		t.Errorf("edited a = %+v", a)
	}
	if store.GetNode("mark://alpha/b.md") == nil {
		t.Error("the unchanged shard's row was dropped")
	}

	// A complete checkpoint drops what it no longer holds.
	hub.checkpoint(t, true, edited)
	seedAlpha(ctx, store, hub)
	if store.GetNode("mark://alpha/b.md") != nil {
		t.Error("a row the checkpoint dropped survived")
	}
}

// An incomplete checkpoint is a subset: absence there is not removal.
func TestSeedKeepsRowsAnIncompleteCheckpointLacks(t *testing.T) {
	ctx, store, hub := context.Background(), New(), newTestHub()
	hub.checkpoint(t, true, seedA, seedB)
	seedAlpha(ctx, store, hub)
	hub.checkpoint(t, false, seedA)
	seedAlpha(ctx, store, hub)
	if store.GetNode("mark://alpha/b.md") == nil {
		t.Error("an incomplete checkpoint removed a row it lacks")
	}
}

// Rows seeded from the world's own graph are never kept as a checkpoint's.
func TestSeedFallsBackToTheWorldsOwnGraph(t *testing.T) {
	ctx, store, hub := context.Background(), New(), newTestHub()
	hub.checkpoint(t, true, seedA, seedB)
	seedAlpha(ctx, store, hub)

	hub.remove(WorldManifestPath("alpha"))
	legacy := BuildExport(time.Now(), []StoredNode{{URL: "mark://alpha/c.md", Title: "C", Status: "ok"}}, nil)
	store.ExpireSeedCheck("alpha")
	store.Seed(ctx, SeedSource{
		Owner: "alpha",
		Fetch: func(_ context.Context, docPath, _ string) (protocol.Response, error) {
			if docPath == LegacyExportPath {
				return protocol.Response{Status: protocol.StatusOK, Body: legacy}, nil
			}
			return protocol.Response{Status: protocol.StatusNotFound}, nil
		},
		Hub: hub.fetch,
	})
	if store.GetNode("mark://alpha/c.md") == nil || store.GetNode("mark://alpha/a.md") != nil {
		t.Fatal("the world's own graph did not replace the checkpoint's rows")
	}

	// The same checkpoint back is read whole: the legacy rows hold none of it.
	reads := hub.shardReads()
	hub.checkpoint(t, true, seedA, seedB)
	seedAlpha(ctx, store, hub)
	if got := hub.shardReads() - reads; got != 2 {
		t.Errorf("read %d shards, want both", got)
	}
	if store.GetNode("mark://alpha/c.md") != nil || store.GetNode("mark://alpha/b.md") == nil {
		t.Error("legacy rows survived the checkpoint, or its rows are missing")
	}
}

// A kept row is found by its decoded path, as the deriver sharded it.
func TestSeedKeepsAnEscapedPathsRow(t *testing.T) {
	spaced := WorldSource{Path: "/b c.md", Version: 1, Title: "Spaced"}
	if SourcePrefix(spaced.Path, 1) == SourcePrefix(seedA.Path, 1) {
		t.Fatal("fixture paths share a shard")
	}
	ctx, store, hub := context.Background(), New(), newTestHub()
	hub.checkpoint(t, true, seedA, spaced)
	seedAlpha(ctx, store, hub)
	edited := seedA
	edited.Version = 2
	hub.checkpoint(t, true, edited, spaced)
	seedAlpha(ctx, store, hub)
	if store.GetNode("mark://alpha/b%20c.md") == nil {
		t.Error("the unchanged shard's escaped row was dropped")
	}
}

// Over rows from the world's own graph, an incomplete checkpoint keeps none:
// they are not a checkpoint's subset.
func TestSeedIncompleteCheckpointKeepsNoFallbackRows(t *testing.T) {
	ctx, store, hub := context.Background(), New(), newTestHub()
	legacy := BuildExport(time.Now(), []StoredNode{{URL: "mark://alpha/c.md", Title: "C", Status: "ok"}}, nil)
	store.Seed(ctx, SeedSource{Owner: "alpha", Hub: hub.fetch, Fetch: func(_ context.Context, docPath, _ string) (protocol.Response, error) {
		if docPath == LegacyExportPath {
			return protocol.Response{Status: protocol.StatusOK, Body: legacy}, nil
		}
		return protocol.Response{Status: protocol.StatusNotFound}, nil
	}})
	hub.checkpoint(t, false, seedA)
	seedAlpha(ctx, store, hub)
	if store.GetNode("mark://alpha/c.md") != nil || store.GetNode("mark://alpha/a.md") == nil {
		t.Error("an incomplete checkpoint kept the fallback's rows")
	}
}

// A checkpoint whose pins all stand, only its cursor moved, rebuilds nothing.
func TestSeedSkipsACheckpointThatChangedNoShard(t *testing.T) {
	ctx, store, hub := context.Background(), New(), newTestHub()
	hub.checkpoint(t, true, seedA, seedB)
	seedAlpha(ctx, store, hub)
	before := fmt.Sprintf("%p", store.nodes)
	hub.checkpointAt(t, protocol.Cursor{Epoch: "e1", Seq: 2}, seedA, seedB)
	seedAlpha(ctx, store, hub)
	if after := fmt.Sprintf("%p", store.nodes); after != before {
		t.Error("a cursor-only checkpoint rebuilt the graph")
	}
	if store.SeedEtag("alpha") != hub.docs[WorldManifestPath("alpha")].Metadata["etag"] {
		t.Error("the new manifest's etag was not recorded")
	}
}
