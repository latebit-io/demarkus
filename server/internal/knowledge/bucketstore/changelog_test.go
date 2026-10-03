package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// publishSeq publishes distinct paths until the log reaches seq.
func publishSeq(t *testing.T, store *Store, seq int64) {
	t.Helper()
	for store.servedSequence() < seq {
		path := fmt.Sprintf("/log/%03d.md", store.servedSequence()+1)
		if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# n\n")}); err != nil {
			t.Fatalf("publish %s: %v", path, err)
		}
	}
}

// behindCheckpoint is a replica whose hub starts at checkpoint, as one does
// after loading a checkpoint there: a resume before it goes through the slots.
func behindCheckpoint(t *testing.T, objects blob.Store, checkpoint int64) *Store {
	t.Helper()
	store := (&bucketSite{objects: objects}).open(t, changefeed.DefaultRingSize)
	store.skipTo(checkpoint)
	return store
}

func resumeAt(t *testing.T, store *Store, since uint64) (*changefeed.Subscription, error) {
	t.Helper()
	return store.Changes().Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: since})
}

// The backlog names every change from the slots, in order.
func TestBacklogResumesThroughSlots(t *testing.T) {
	objects := initializedMemory(t)
	publishSeq(t, openReplica(t, objects).store, 12)
	sub, err := resumeAt(t, behindCheckpoint(t, objects, 9), 3)
	if err != nil {
		t.Fatalf("resume behind the checkpoint: %v", err)
	}
	for seq := uint64(4); seq <= 9; seq++ {
		ev := storetest.NextEvent(t, sub)
		if ev.Seq != seq || ev.Path != fmt.Sprintf("/log/%03d.md", seq) || ev.Op != protocol.OpPublish || ev.Version != 1 {
			t.Fatalf("event %d = %+v", seq, ev)
		}
	}
}

// A slot that is missing or corrupt resyncs the watcher instead of guessing.
func TestUnusableSlotResyncs(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, objects *blob.Memory)
		want   error
	}{
		{name: "missing", want: blob.ErrNotFound, damage: func(t *testing.T, objects *blob.Memory) {
			deleteObject(t, objects, slotKey(5))
		}},
		{name: "corrupt", want: blob.ErrIntegrity, damage: func(t *testing.T, objects *blob.Memory) {
			rewriteSlot(t, objects, 5, func(slot *slotObject) { slot.First++ })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := initializedMemory(t)
			publishSeq(t, openReplica(t, objects).store, 12)
			reader := behindCheckpoint(t, objects, 9)
			tt.damage(t, objects)
			if _, err := resumeAt(t, reader, 3); !errors.Is(err, changefeed.ErrResync) || !errors.Is(err, tt.want) {
				t.Fatalf("resume over the damaged slot: %v, want ErrResync with %v", err, tt.want)
			}
			if _, err := resumeAt(t, reader, 6); err != nil {
				t.Fatalf("resume after the damaged slot: %v", err)
			}
		})
	}
}

// rewriteSlot replaces the slot at first with edit applied to it.
func rewriteSlot(t *testing.T, objects blob.Store, first int64, edit func(*slotObject)) {
	t.Helper()
	object := getObject(t, objects, slotKey(first))
	var slot slotObject
	decodeObject(t, object.Data, &slot)
	edit(&slot)
	data, err := marshalImmutable(slot)
	if err != nil {
		t.Fatal(err)
	}
	replaceObject(t, objects, slotKey(first), object.Attributes.Generation, data)
}

// Slots are immutable, so resumes share them: a second read of the same range
// costs the bucket no slot read, and the cache stays within its limit.
func TestBacklogCachesSlots(t *testing.T) {
	objects := newObservedBlobStore(initializedMemory(t))
	publishSeq(t, openReplica(t, objects).store, 20)
	reader := (&bucketSite{objects: objects}).open(t, 0)
	backlog := newChangeLog(reader, maxSlotEntries, backlogReach, slotCacheIdle)
	events, err := backlog.Events(t.Context(), 10, 14)
	if err != nil || len(events) != 4 || events[0].Seq != 11 || events[3].Seq != 14 {
		t.Fatalf("events (10,14] = %+v, %v", events, err)
	}
	read := countPrefix(objects.counts().gets, logPrefix)
	if _, err := backlog.Events(t.Context(), 10, 14); err != nil {
		t.Fatal(err)
	}
	if countPrefix(objects.counts().gets, logPrefix) != read {
		t.Fatal("a repeated range read the bucket again")
	}
	if backlog.held > backlog.limit {
		t.Fatalf("cache holds %d events, limit %d", backlog.held, backlog.limit)
	}
}

// publishBatched publishes n documents while the first slot's create is
// held, so the rest queue and commit in batched slots.
func publishBatched(t *testing.T, objects blob.Store, n int) *Store {
	t.Helper()
	holds := newSlotHolds(t, objects, 2)
	writer := (&bucketSite{objects: holds}).open(t, 0)
	publish := func(index int) {
		path := fmt.Sprintf("/agents/session-%d/inbox/01HZX%08d.md", index%7, index)
		meta := map[string]string{"agent": fmt.Sprintf("federation-agent-%d-2026-09-30T15:58:33.%09dZ", index%3, index)}
		if _, err := writer.Publish(context.Background(), backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: []byte("# n\n"), Metadata: meta}); err != nil {
			t.Errorf("publish %s: %v", path, err)
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { publish(0) })
	waitForTestSignal(t, holds.holds[2].arrived, "the first slot's create")
	for index := 1; index < n; index++ {
		wg.Go(func() { publish(index) })
	}
	waitUntil(t, writer, "half the writes queued", func(queue *commitQueue) bool { return len(queue.waiting) >= n/2 })
	holds.holds[2].open()
	wg.Wait()
	return writer
}

// A resume may start at most reach slots behind the head, counted in slots,
// so batched slots let it reach further back in changes.
func TestBacklogReachCountsSlots(t *testing.T) {
	objects := initializedMemory(t)
	writer := publishBatched(t, objects, 100)
	head := writer.servedSequence()
	slots := uint64(len(logSlots(t, objects, head)))
	if lag := uint64(head) - 1; lag <= slots-1 || lag > (slots-1)*maxSlotEntries {
		t.Fatalf("%d slots for %d changes; the test needs batched slots", slots, lag)
	}
	for _, tt := range []struct {
		reach uint64
		want  error
	}{{reach: slots}, {reach: slots - 1, want: errBeyondReach}} {
		backlog := newChangeLog(writer, 8, tt.reach, slotCacheIdle)
		if err := backlog.Reaches(t.Context(), 1, uint64(head)); !errors.Is(err, tt.want) {
			t.Errorf("Reaches over %d slots with a reach of %d: %v, want %v", slots, tt.reach, err, tt.want)
		}
	}
}

// An idle slot cache empties itself: a world nobody resumes on keeps none of
// the agent-shaped events its last resumes read.
func TestSlotCacheEmptiesWhenIdle(t *testing.T) {
	objects := initializedMemory(t)
	head := uint64(publishBatched(t, objects, 600).servedSequence())
	reader := (&bucketSite{objects: objects, idle: 20 * time.Millisecond}).open(t, changefeed.DefaultRingSize)
	reader.skipTo(int64(head))
	held := func() int {
		reader.changeLog.mu.Lock()
		defer reader.changeLog.mu.Unlock()
		return reader.changeLog.held
	}
	growth := memtest.Retained(func() {
		sub, err := resumeAt(t, reader, 1)
		mustSucceed(t, err)
		for seq := uint64(2); seq <= head; seq++ {
			storetest.NextEvent(t, sub)
		}
		if held() == 0 {
			t.Fatal("the resume cached no slots")
		}
		waitFor(t, "an empty slot cache", func() bool { return held() == 0 })
	})
	t.Logf("heap grew %d bytes after the cache went idle", growth)
	if growth > 32<<10 {
		t.Errorf("heap grew %d bytes once the slot cache went idle, want under 32 KiB", growth)
	}
}

// slowSlots delays every slot read, so concurrent readers overlap.
type slowSlots struct{ *observedBlobStore }

func (s slowSlots) Get(ctx context.Context, key string) (blob.Object, error) {
	if strings.HasPrefix(key, logPrefix) {
		time.Sleep(50 * time.Millisecond)
	}
	return s.observedBlobStore.Get(ctx, key)
}

// Resumes over the same slots at once share each slot's read, as after a
// network blip reconnects every watcher from about the same cursor.
func TestConcurrentResumesShareSlotReads(t *testing.T) {
	objects := initializedMemory(t)
	publishSeq(t, (&bucketSite{objects: objects}).open(t, 0), 20)
	observed := newObservedBlobStore(objects)
	reader := (&bucketSite{objects: slowSlots{observed}}).open(t, 0)
	backlog := newChangeLog(reader, maxSlotEntries, backlogReach, slotCacheIdle)
	observed.reset()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			if events, err := backlog.Events(t.Context(), 1, 20); err != nil || len(events) != 19 {
				t.Errorf("events (1,20] = %d events, %v", len(events), err)
			}
		})
	}
	close(start)
	wg.Wait()
	if gets := countPrefix(observed.counts().gets, logPrefix); gets != 19 {
		t.Errorf("8 concurrent resumes read %d slots, want each of the 19 once", gets)
	}
}
