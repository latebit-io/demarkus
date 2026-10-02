package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
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
	backlog := newChangeLog(reader, maxSlotEntries)
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
