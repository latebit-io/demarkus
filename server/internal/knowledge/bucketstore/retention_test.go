package bucketstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// slotsAt publishes to sequence 16 one slot at a time, written age ago,
// through checkpointedLog.
func slotsAt(t *testing.T, age time.Duration) (*clockedStore, *Store) {
	t.Helper()
	objects := newClockedStore(t)
	objects.writtenAgo(age)
	writer := manualSite(objects).open(t, 2)
	checkpointedLog(t, writer)
	return objects, writer
}

// checkpointedLog publishes to sequence 16, checkpointing at 10, 12, 14 and
// 16: the newest three keep 12 onwards.
func checkpointedLog(t *testing.T, writer *Store) {
	t.Helper()
	for _, sequence := range []int64{10, 12, 14, 16} {
		publishSeq(t, writer, sequence)
		mustSucceed(t, writer.checkpoint(context.Background()))
	}
}

// A slot goes once the oldest kept checkpoint covers it and it is past the
// retention; younger slots, and those after the oldest kept checkpoint, stay.
func TestSlotsGoAfterTheRetention(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		gone int64 // slots 2 through gone are collected
	}{
		{name: "past the retention", age: slotRetention + time.Hour, gone: 12},
		{name: "within the retention", age: slotRetention - time.Hour, gone: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, _ := slotsAt(t, tt.age)
			for first := int64(2); first <= 16; first++ {
				if kept := exists(t, objects, slotKey(first)); kept == (first <= tt.gone) {
					t.Errorf("slot %d kept = %v, want slots through %d collected", first, kept, tt.gone)
				}
			}
		})
	}
}

// A cursor whose next change was collected resyncs; one just after resumes
// from the slots that remain.
func TestResumePastTheRetentionResyncs(t *testing.T) {
	_, writer := slotsAt(t, slotRetention+time.Hour)
	if _, err := resumeAt(t, writer, 11); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("resume before the retained slots: %v, want ErrResync", err)
	}
	if _, err := resumeAt(t, writer, 12); err != nil {
		t.Fatalf("resume at the oldest retained slot: %v", err)
	}
}

// A replica unconfirmed for half the retention reloads from the newest
// checkpoint before trusting a missing slot as the tip, or it would commit
// into a collected slot's name; its section index resyncs across the reload.
func TestStaleReplicaReloadsBeforeTrustingAMissingSlot(t *testing.T) {
	objects := newClockedStore(t)
	objects.writtenAgo(slotRetention + time.Hour)
	stale := manualSite(objects).open(t, 0)
	assertRows(t, stale, "haystack")
	writer := manualSite(objects).open(t, 0)
	write(t, writer, "/needle.md", "# Needle\n\nhaystack\n", nil)
	checkpointedLog(t, writer)
	if exists(t, objects, slotKey(2)) {
		t.Fatal("slot 2 was not collected; the test needs it gone")
	}
	unconfirmed(stale)

	if _, err := stale.Get("/log/016.md", 0); err != nil {
		t.Fatalf("read on the stale replica: %v", err)
	}
	doc, err := stale.Publish(context.Background(), backend.WriteRequest{Path: "/after.md", ExpectedVersion: -1, Content: []byte("# after\n")})
	if err != nil {
		t.Fatalf("publish on the stale replica: %v", err)
	}
	if got := stale.servedSequence(); got != 17 || doc.Version != 1 {
		t.Fatalf("stale replica committed at sequence %d, want 17 after the log", got)
	}
	assertRows(t, stale, "haystack", "/needle.md#needle")
}

// A watch open on a replica that reloads past collected slots is told to
// resync, never handed the next event as if nothing came between.
func TestStaleReloadResyncsOpenWatches(t *testing.T) {
	objects := newClockedStore(t)
	objects.writtenAgo(slotRetention + time.Hour)
	stale := manualSite(objects).open(t, changefeed.DefaultRingSize)
	sub, err := stale.Changes().Subscribe(t.Context(), "/", protocol.Cursor{})
	mustSucceed(t, err)
	checkpointedLog(t, manualSite(objects).open(t, 0))
	unconfirmed(stale)

	_, err = stale.Publish(context.Background(), backend.WriteRequest{Path: "/after.md", ExpectedVersion: -1, Content: []byte("# after\n")})
	mustSucceed(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if ev, err := sub.Next(ctx); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("open watch after the reload: %+v, %v; want ErrResync for the collected changes", ev, err)
	}
}

// unconfirmed marks store's snapshot as unconfirmed for longer than staleAfter.
func unconfirmed(store *Store) {
	current := store.served.Load()
	store.served.Store(&served{snap: current.snap, confirmed: time.Now().Add(-staleAfter - time.Minute)})
}
