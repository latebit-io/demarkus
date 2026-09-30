package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// publishSeq publishes distinct paths until the head reaches seq.
func publishSeq(t *testing.T, store *Store, seq int64) {
	t.Helper()
	for store.HeadSequence() < seq {
		path := fmt.Sprintf("/log/%03d.md", store.HeadSequence()+1)
		if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# n\n")}); err != nil {
			t.Fatalf("publish %s: %v", path, err)
		}
	}
}

// resumeFrom subscribes a freshly opened replica at seq, as a watcher does
// after the replica it watched restarted.
func resumeFrom(t *testing.T, objects blob.Store, seq uint64) (*changefeed.Subscription, error) {
	t.Helper()
	store := (&bucketSite{objects: objects}).open(t, changefeed.DefaultRingSize)
	return store.Changes().Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: seq})
}

// A seal lost with its commit is completed by the next commit, whose
// receipts still cover the block, and a restart then resumes through it.
func TestLostSealIsCompletedByTheNextCommit(t *testing.T) {
	// The first seal attempt fails, as a bucket outage between a commit and
	// its seal does.
	objects := &failFirstPrefixCreateStore{Store: initializedMemory(t), prefix: objectPrefix + "changes/"}
	writer := openReplica(t, objects).store
	publishSeq(t, writer, changeBlockSize)
	if _, err := objects.Get(context.Background(), changeBlockKey(0)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("block 0 after a refused seal: %v, want not found", err)
	}
	publishSeq(t, writer, 3*changeBlockSize)

	sub, err := resumeFrom(t, objects, 1)
	if err != nil {
		t.Fatalf("resume across the receipt window: %v", err)
	}
	for seq := uint64(2); seq <= 3*changeBlockSize; seq++ {
		ev := storetest.NextEvent(t, sub)
		if ev.Seq != seq || ev.Path != fmt.Sprintf("/log/%03d.md", seq) || ev.Op != protocol.OpPublish {
			t.Fatalf("event %d = %+v", seq, ev)
		}
	}
}

// Replicas that seal one block write identical bytes, so the second seal
// is a verified no-op rather than a conflict.
func TestReplicasSealOneBlockIdentically(t *testing.T) {
	objects := initializedMemory(t)
	a := openReplica(t, objects).store
	peer := openReplica(t, objects)
	b := peer.store
	publishSeq(t, a, changeBlockSize)
	if a.sealedThrough.Load() != changeBlockSize {
		t.Fatalf("writer sealed through %d, want %d", a.sealedThrough.Load(), changeBlockSize)
	}
	peer.poll(t)
	publishSeq(t, b, changeBlockSize+1)
	if b.sealedThrough.Load() != changeBlockSize {
		t.Fatalf("second writer sealed through %d, want %d: resealing the block failed", b.sealedThrough.Load(), changeBlockSize)
	}
}

// A block that is missing, corrupt, or names no change resyncs the watcher
// instead of guessing.
func TestUnusableBlockResyncs(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, objects *blob.Memory)
		want   error
	}{
		{name: "never sealed", want: blob.ErrNotFound, damage: func(t *testing.T, objects *blob.Memory) {
			deleteObject(t, objects, changeBlockKey(0))
		}},
		{name: "corrupt", want: blob.ErrIntegrity, damage: func(t *testing.T, objects *blob.Memory) {
			rewriteBlock(t, objects, 0, func(block *changeBlock) { block.Receipts[0].Sequence++ })
		}},
		{name: "unnamed receipt", damage: func(t *testing.T, objects *blob.Memory) {
			rewriteBlock(t, objects, 0, func(block *changeBlock) {
				receipt := &block.Receipts[1]
				*receipt = operationReceipt{OperationID: receipt.OperationID, Sequence: receipt.Sequence, Result: "committed"}
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := initializedMemory(t)
			publishSeq(t, openReplica(t, objects).store, 3*changeBlockSize)
			tt.damage(t, objects)
			_, err := resumeFrom(t, objects, 1)
			if !errors.Is(err, changefeed.ErrResync) {
				t.Fatalf("resume: %v, want ErrResync", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("resume: %v, want %v", err, tt.want)
			}
			if _, err := resumeFrom(t, objects, changeBlockSize); err != nil {
				t.Fatalf("resume after the damaged block: %v", err)
			}
		})
	}
}

func readBlock(t *testing.T, objects blob.Store, index int64) (changeBlock, blob.Generation) {
	t.Helper()
	object := getObject(t, objects, changeBlockKey(index))
	var block changeBlock
	decodeObject(t, object.Data, &block)
	return block, object.Attributes.Generation
}

// rewriteBlock replaces block index with edit applied to it.
func rewriteBlock(t *testing.T, objects blob.Store, index int64, edit func(*changeBlock)) {
	t.Helper()
	block, generation := readBlock(t, objects, index)
	edit(&block)
	data, err := marshalImmutable(block)
	if err != nil {
		t.Fatal(err)
	}
	replaceObject(t, objects, changeBlockKey(index), generation, data)
}

// blockReads counts bucket reads of sealed change blocks.
type blockReads struct {
	blob.Store
	reads atomic.Int64
}

func (s *blockReads) Get(ctx context.Context, key string) (blob.Object, error) {
	if strings.HasPrefix(key, objectPrefix+"changes/") {
		s.reads.Add(1)
	}
	return s.Store.Get(ctx, key)
}

// Sealed blocks are immutable, so resumes share them: a second resume over
// the same range costs the bucket nothing, and the cache stays within a
// ring's worth of blocks.
func TestBacklogCachesSealedBlocks(t *testing.T) {
	objects := &blockReads{Store: initializedMemory(t)}
	publishSeq(t, openReplica(t, objects).store, 5*changeBlockSize)
	const ring = 4 * changeBlockSize
	reader := (&bucketSite{objects: objects}).open(t, ring)
	resume := func(since uint64) {
		t.Helper()
		sub, err := reader.Changes().Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: since})
		if err != nil {
			t.Fatalf("resume at %d: %v", since, err)
		}
		storetest.NextEvent(t, sub)
	}
	// The receipts cover 25 to 40, so a resume from 16 needs block 2 only.
	resume(2 * changeBlockSize)
	resume(2 * changeBlockSize)
	if n := objects.reads.Load(); n != 1 {
		t.Fatalf("two resumes over one block read the bucket %d times, want 1", n)
	}
	backlog := reader.backlog
	if backlog.recent.Len() != 1 || backlog.limit != ring/changeBlockSize {
		t.Fatalf("cache holds %d blocks with limit %d, want 1 and %d", backlog.recent.Len(), backlog.limit, ring/changeBlockSize)
	}
}
