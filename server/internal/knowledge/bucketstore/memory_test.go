package bucketstore

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/memtest"
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

// The agent's publish-and-prune loop, the write path that leaked in
// production, must leave heap tracking the live documents, not the writes.
func TestPublishPruneHeapTracksLiveData(t *testing.T) {
	memory, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	objects := &storedBytes{Store: memory}
	ctx := context.Background()
	if err := Initialize(ctx, objects, testWorldID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	store, err := Open(ctx, objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.commitInterval = 0
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
