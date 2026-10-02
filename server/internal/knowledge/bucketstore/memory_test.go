package bucketstore

import (
	"context"
	"fmt"
	"runtime"
	"sync"
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
	cycle := func(t *testing.T, store *Store, n int) {
		publishPruned(t, store, "/graph.md", memtest.AgentGraphBody(n))
		for server := range 5 {
			publishPruned(t, store, fmt.Sprintf("/index/server-%d.md", server), fmt.Appendf(nil, "# Index %d\n\nCycle %d.\n", server, n))
		}
	}
	t.Run("plain", func(t *testing.T) { checkCommitHeap(t, 0, cycle) })
	t.Run("watch", func(t *testing.T) { checkCommitHeap(t, 16, cycle) })
}

// Concurrent writers fill the committer's queue, batches and pipeline; once
// they drain and the committer stops, heap tracks the live documents alone.
func TestConcurrentCommitHeapTracksLiveData(t *testing.T) {
	checkCommitHeap(t, 0, func(t *testing.T, store *Store, n int) {
		var wg sync.WaitGroup
		for writer := range 24 {
			wg.Go(func() {
				body := fmt.Appendf(nil, "# Index %d\n\nCycle %d.\n", writer, n)
				if writer == 0 {
					body = memtest.AgentGraphBody(n)
				}
				publishPruned(t, store, fmt.Sprintf("/w/%d.md", writer), body)
			})
		}
		wg.Wait()
	})
}

// publishPruned publishes body under the agent's retention, so every write
// past warmup also prunes.
func publishPruned(t *testing.T, store *Store, path string, body []byte) {
	meta := map[string]string{"retention": "20", "agent": "federation"}
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: body, Metadata: meta}); err != nil {
		t.Errorf("publish %s: %v", path, err)
	}
}

// checkCommitHeap runs cycle past retention, then measures it: heap outside
// what the in-memory bucket holds must stay under two graph bodies.
func checkCommitHeap(t *testing.T, ring int, cycle func(t *testing.T, store *Store, n int)) {
	memory, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	objects := &storedBytes{Store: memory}
	ctx := context.Background()
	if err := initialize(ctx, objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	store, err := Open(ctx, objects, Options{Logger: discardLogger, WorldID: testWorldID, ChangeRing: ring, trigger: manual})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	const warmup, cycles = 25, 60
	for n := range warmup {
		cycle(t, store, n)
	}
	waitIdle(t, store)
	bytesBefore, objectsBefore := objects.bytes.Load(), objects.objects.Load()
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			cycle(t, store, n)
		}
		waitIdle(t, store)
	})
	runtime.KeepAlive(store)
	// Each stored object also costs its key and map slot beyond its bytes.
	bucket := objects.bytes.Load() - bytesBefore + 512*(objects.objects.Load()-objectsBefore)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes, %d of them the bucket's", growth, bucket)
	if limit := 2 * bodyBytes; growth-bucket > limit {
		t.Errorf("heap outside the bucket grew %d bytes over %d cycles, want under %d", growth-bucket, cycles, limit)
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
