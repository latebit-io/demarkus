package bucketstore

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// storedBytes counts the object bytes a blob store holds, so a heap reading
// can leave out what the in-memory bucket keeps by design.
type storedBytes struct {
	blob.Store
	bytes   atomic.Int64
	objects atomic.Int64
}

func (s *storedBytes) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := s.Store.Create(ctx, key, data)
	if err == nil {
		s.bytes.Add(int64(len(data)))
		s.objects.Add(1)
	}
	return attributes, err
}

func (s *storedBytes) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	previous, err := s.Head(ctx, key)
	if err != nil {
		return blob.Attributes{}, err
	}
	attributes, err := s.Store.Replace(ctx, key, generation, data)
	if err == nil {
		s.bytes.Add(int64(len(data)) - previous.Size)
	}
	return attributes, err
}

// The agent's publish-and-prune loop, which leaked in production, must leave
// heap tracking live documents: retention trims the versions a snapshot holds
// and old snapshots share nothing pinned. With WATCH on, the hub must evict.
func TestPublishPruneHeapTracksLiveData(t *testing.T) {
	t.Run("plain", func(t *testing.T) { checkPublishPruneHeap(t, 0) })
	t.Run("watch", func(t *testing.T) { checkPublishPruneHeap(t, 16) })
}

func checkPublishPruneHeap(t *testing.T, ring int) {
	memory, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	objects := &storedBytes{Store: memory}
	ctx := context.Background()
	if err := initialize(ctx, objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	store, err := Open(ctx, objects, Options{Logger: discardLogger, WorldID: testWorldID, ChangeRing: ring})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	meta := map[string]string{"retention": "20", "agent": "federation"}
	cycle := func(n int) {
		if _, err := store.Publish(ctx, backend.WriteRequest{Path: "/graph.md", ExpectedVersion: -1, Content: memtest.AgentGraphBody(n), Metadata: meta}); err != nil {
			t.Fatalf("cycle %d graph: %v", n, err)
		}
		for server := range 5 {
			body := fmt.Appendf(nil, "# Index %d\n\nCycle %d.\n", server, n)
			if _, err := store.Publish(ctx, backend.WriteRequest{Path: fmt.Sprintf("/index/server-%d.md", server), ExpectedVersion: -1, Content: body, Metadata: meta}); err != nil {
				t.Fatalf("cycle %d index: %v", n, err)
			}
		}
	}
	// Past retention, so every measured write also prunes.
	const warmup, cycles = 25, 60
	for n := range warmup {
		cycle(n)
	}
	bytesBefore, objectsBefore := objects.bytes.Load(), objects.objects.Load()
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			cycle(n)
		}
	})
	runtime.KeepAlive(store)
	// Each stored object also costs its key and map slot beyond its bytes.
	bucket := objects.bytes.Load() - bytesBefore + 512*(objects.objects.Load()-objectsBefore)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes, %d of them the bucket's", growth, bucket)
	if limit := 2 * bodyBytes; growth-bucket > limit {
		t.Errorf("heap outside the bucket grew %d bytes over %d publish-and-prune cycles of a %d-byte body, want under %d", growth-bucket, cycles, bodyBytes, limit)
	}
}

// A watcher resuming through the slots, again and again, keeps nothing once
// it has drained and gone: the backlog lives only as long as its watch, and
// the slot cache stays within its limit.
func TestBacklogResumeRetainsNothing(t *testing.T) {
	objects := initializedMemory(t)
	writer := (&bucketSite{objects: objects}).open(t, 0)
	publishSeq(t, writer, 200)
	// A hub that starts at 199, as after a checkpoint there, reads every
	// resume from seq 100 out of the slots.
	reader := (&bucketSite{objects: objects}).open(t, changefeed.DefaultRingSize)
	reader.skipTo(199)
	hub := reader.Changes()
	resume := func() {
		sub, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: 100})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		for range 100 {
			storetest.NextEvent(t, sub)
		}
	}
	// Warmup grows the runtime's threads and fills the slot cache; both are
	// reused, not per resume.
	for range 50 {
		resume()
	}
	const resumes = 200
	growth := memtest.Retained(func() {
		for range resumes {
			resume()
		}
	})
	runtime.KeepAlive(reader)
	t.Logf("heap grew %d bytes over %d resumes", growth, resumes)
	// A retained backlog costs about 17 KB a resume, 3.4 MB here.
	if growth > 512<<10 {
		t.Errorf("heap grew %d bytes over %d drained resumes, want under 512 KiB", growth, resumes)
	}
}
