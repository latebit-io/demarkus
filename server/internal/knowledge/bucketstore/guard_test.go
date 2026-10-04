package bucketstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// heldReads holds reads under prefix until released, counting them.
type heldReads struct {
	blob.Store
	prefix string
	open   chan struct{}
	reads  atomic.Int64
}

func (s *heldReads) Get(ctx context.Context, key string) (blob.Object, error) {
	if strings.HasPrefix(key, s.prefix) {
		s.reads.Add(1)
		select {
		case <-s.open:
		case <-ctx.Done():
			return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
		}
	}
	return s.Store.Get(ctx, key)
}

// A stale reload is shared and outlives the request that asked: one whose
// deadline passes mid-load fails alone, and the next finds the load done
// rather than starting it again.
func TestStaleReloadOutlivesItsRequest(t *testing.T) {
	objects := newClockedStore(t)
	objects.writtenAgo(slotRetention + time.Hour)
	held := &heldReads{Store: objects, prefix: checkpointPrefix, open: make(chan struct{})}
	close(held.open)
	stale := manualSite(held).open(t, 0)
	checkpointedLog(t, manualSite(objects).open(t, 0))
	unconfirmed(stale)

	held.open = make(chan struct{})
	held.reads.Store(0)
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := stale.refresh(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh past its deadline: %v, want DeadlineExceeded", err)
	}
	close(held.open)
	if _, err := stale.Get("/log/016.md", 0); err != nil {
		t.Fatalf("read after the reload: %v", err)
	}
	if n := held.reads.Load(); n != 1 {
		t.Errorf("the newest checkpoint was read %d times, want once for one shared reload", n)
	}
}

// A slot create that finds its own bytes never rewrites them, however old
// they look: a slot is written once.
func TestSlotCreateNeverRewrites(t *testing.T) {
	objects := newClockedStore(t)
	objects.writtenAgo(time.Hour)
	slot := modelObject{Key: slotKey(2), Data: []byte(`{}`)}
	_, err := createSlot(context.Background(), objects, slot)
	mustSucceed(t, err)
	before := getObject(t, objects, slot.Key).Attributes.Generation
	_, err = createSlot(context.Background(), objects, slot)
	mustSucceed(t, err)
	if after := getObject(t, objects, slot.Key).Attributes.Generation; after != before {
		t.Fatalf("slot generation moved from %d to %d: the create rewrote it", before, after)
	}
}

// A compactor whose snapshot failed to rebase on the newest checkpoint
// writes nothing: its shard references could name collected objects.
func TestCompactorRefusesAnUnrebasedSnapshot(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	peer := manualSite(objects).open(t, 0)
	writeWorld(t, writer, 0)
	mustSucceed(t, peer.poll(ctx))
	checkpointOnly(t, peer)
	// An adoption whose rebase fails, as one ahead of the log does: the
	// served snapshot stays on the older checkpoint.
	writer.adoption.Store(&adoption{checkpoint: peer.layout(), entries: map[string]*baseEntry{"/never.md": {Current: 1}}})
	// One write installs the unrebased snapshot; no refresh reloads it yet.
	_, err := writer.WriteVersion("/after.md", -1, []byte("# after\n"), nil)
	mustSucceed(t, err)
	if err := writer.checkpoint(ctx); !errors.Is(err, blob.ErrIntegrity) {
		t.Fatalf("checkpoint on an unrebased snapshot: %v, want ErrIntegrity", err)
	}
	if newest, err := newestCheckpointSequence(ctx, objects, 0); err != nil || newest != peer.layout().Sequence {
		t.Fatalf("newest checkpoint %d, %v; want the peer's %d alone", newest, err, peer.layout().Sequence)
	}
}

// An adoption older than the checkpoint the served snapshot rests on, as
// after a reload, is ignored: the store's layout never goes backwards.
func TestOlderAdoptionIsIgnored(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	writeWorld(t, writer, 0)
	checkpointOnly(t, writer)
	older := writer.layout()
	writeWorld(t, writer, 1)
	checkpointOnly(t, writer)
	reloaded := manualSite(objects).open(t, 0)
	mustSucceed(t, reloaded.poll(ctx))
	newest := reloaded.layout().Sequence
	reloaded.installAdoption(&adoption{checkpoint: older})
	if got := reloaded.layout().Sequence; got != newest {
		t.Fatalf("layout went back to checkpoint %d from %d", got, newest)
	}
}

// A closed store starts no warm-up, and a write it refuses leaves none
// behind; a finished warm-up lets go of the batch it read through.
func TestWarmupsEndWithTheStore(t *testing.T) {
	store, _ := newWritableStore(t)
	_, err := store.WriteVersion("/a.md", -1, []byte("# a\n"), nil)
	mustSucceed(t, err)
	w := store.warm(context.Background(), "/a.md")
	if w == nil {
		t.Fatal("no warm-up for an existing document")
	}
	<-w.done
	if w.objects.source != nil {
		t.Error("a finished warm-up still reads through the newest batch")
	}
	store.unwarm(&commitRequest{path: "/a.md", warm: w})
	// Close refuses writes before it ends background work: a write in that
	// gap starts a warm-up and must not leave it behind.
	store.commits.close()
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/a.md", ExpectedVersion: -1, Content: []byte("# b\n")}); !errors.Is(err, backend.ErrClosed) {
		t.Fatalf("publish after the queue closed: %v, want ErrClosed", err)
	}
	store.warmMu.Lock()
	left := len(store.warming)
	store.warmMu.Unlock()
	if left != 0 {
		t.Errorf("%d warm-ups left behind by a refused write", left)
	}
	mustSucceed(t, store.Close())
	if store.warm(context.Background(), "/a.md") != nil {
		t.Error("a closed store started a warm-up")
	}
}

// An adoption's entries rebase snapshots built before it; once the served
// snapshot rests on it and no committer runs, the store keeps only its layout.
func TestAdoptionReleasesItsEntries(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	peer := manualSite(objects).open(t, 0)
	writeWorld(t, writer, 0)
	checkpointOnly(t, writer)
	writeWorld(t, writer, 1)
	checkpointOnly(t, writer)
	mustSucceed(t, peer.poll(ctx))
	mustSucceed(t, peer.checkpoint(ctx))
	adopted := peer.adoption.Load()
	if adopted == nil || adopted.checkpoint.Sequence != writer.layout().Sequence {
		t.Fatalf("peer adopted %+v, want the writer's checkpoint %d", adopted, writer.layout().Sequence)
	}
	if adopted.entries != nil {
		t.Errorf("an idle peer kept %d adoption entries it no longer needs", len(adopted.entries))
	}
	readEveryVersion(t, peer)
}

// slotGate holds reads of one key while shut, as a stalled bucket call does.
type slotGate struct {
	blob.Store
	key  string
	mu   sync.Mutex
	shut chan struct{} // nil while open
	held atomic.Int64
}

func (g *slotGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut = make(chan struct{})
}

func (g *slotGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		close(g.shut)
		g.shut = nil
	}
}

func (g *slotGate) Get(ctx context.Context, key string) (blob.Object, error) {
	if key == g.key {
		g.mu.Lock()
		shut := g.shut
		g.mu.Unlock()
		if shut != nil {
			g.held.Add(1)
			select {
			case <-shut:
			case <-ctx.Done():
				return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
			}
		}
	}
	return g.Store.Get(ctx, key)
}

// A commit confirms its slot while a read's probe of the log is stalled on
// the bucket: the probe holds no lock an install needs.
func TestCommitConfirmsWhileAProbeStalls(t *testing.T) {
	ctx := context.Background()
	gate := &slotGate{Store: initializedMemory(t), key: slotKey(3)}
	holds := newSlotHolds(t, gate, 3)
	store := (&bucketSite{objects: holds, noHedge: true}).open(t, 0)
	_, err := store.WriteVersion("/a.md", -1, []byte("# a\n"), nil)
	mustSucceed(t, err)

	written := publishAsync(ctx, store, backend.WriteRequest{Path: "/b.md", ExpectedVersion: -1, Content: []byte("# b\n")})
	waitForTestSignal(t, holds.holds[3].arrived, "the create of slot 3")
	gate.close()
	read := make(chan error, 1)
	go func() {
		_, err := store.Get("/a.md", 0)
		read <- err
	}()
	waitFor(t, "the read's probe of slot 3 to stall", func() bool { return gate.held.Load() > 0 })
	holds.holds[3].open()
	select {
	case outcome := <-written:
		mustSucceed(t, outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("the commit waited on a probe stalled on the bucket")
	}
	gate.open()
	mustSucceed(t, <-read)
	if got := store.servedSequence(); got != 3 {
		t.Errorf("served sequence %d after the commit and the read, want 3", got)
	}
}

// A commit confirms its slot while a reload's replay is stalled on the
// bucket: the replay holds no lock an install needs, and the reload replays on
// past the commit before it installs.
func TestCommitConfirmsWhileAReloadReplays(t *testing.T) {
	ctx := context.Background()
	gate := &slotGate{Store: initializedMemory(t), key: slotKey(3)}
	holds := newSlotHolds(t, gate, 5)
	store := (&bucketSite{objects: holds, trigger: manual, noHedge: true}).open(t, 0)
	_, err := store.WriteVersion("/a.md", -1, []byte("# a\n"), nil)
	mustSucceed(t, err)
	mustSucceed(t, store.checkpoint(ctx))
	for _, path := range []string{"/b.md", "/c.md"} {
		_, err := store.WriteVersion(path, -1, []byte("# x\n"), nil)
		mustSucceed(t, err)
	}

	written := publishAsync(ctx, store, backend.WriteRequest{Path: "/d.md", ExpectedVersion: -1, Content: []byte("# d\n")})
	waitForTestSignal(t, holds.holds[5].arrived, "the create of slot 5")
	gate.close()
	store.diverged.Store(true)
	read := make(chan error, 1)
	go func() {
		_, err := store.Get("/a.md", 0)
		read <- err
	}()
	waitFor(t, "the reload's replay of slot 3 to stall", func() bool { return gate.held.Load() > 0 })
	holds.holds[5].open()
	select {
	case outcome := <-written:
		mustSucceed(t, outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("the commit waited on a reload's replay stalled on the bucket")
	}
	gate.open()
	mustSucceed(t, <-read)
	if got := store.servedSequence(); got != 5 {
		t.Errorf("served sequence %d after the reload and the commit, want 5", got)
	}
	if store.diverged.Load() {
		t.Error("the reload left the store diverged")
	}
	readEveryVersion(t, store)
}

// A snapshot that failed to rebase on the newest checkpoint is replaced at
// the next refresh by a reload from it, and the compactor writes again.
func TestFailedRebaseReloads(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	// A step bound of a nanosecond: the writer takes over from the peer's
	// fresh checkpoint without deferring to it.
	writer := (&bucketSite{objects: objects, trigger: stepping(0, time.Nanosecond)}).open(t, 0)
	peer := manualSite(objects).open(t, 0)
	writeWorld(t, writer, 0)
	mustSucceed(t, peer.poll(ctx))
	checkpointOnly(t, peer)
	writer.adoption.Store(&adoption{checkpoint: peer.layout(), entries: map[string]*baseEntry{"/never.md": {Current: 1}}})
	writeWorld(t, writer, 1)
	mustSucceed(t, writer.poll(ctx))
	if writer.diverged.Load() || writer.served.Load().snap.Checkpoint.Sequence != peer.layout().Sequence {
		t.Fatalf("after a failed rebase the store rests on checkpoint %d, want a reload onto the peer's %d", writer.served.Load().snap.Checkpoint.Sequence, peer.layout().Sequence)
	}
	mustSucceed(t, writer.checkpoint(ctx))
	if writer.layout().Sequence <= peer.layout().Sequence {
		t.Errorf("compactor wrote nothing after the reload: newest checkpoint %d", writer.layout().Sequence)
	}
	readEveryVersion(t, writer)
}

// A warm-up waits for no slot: with every slot busy, the build reads for
// itself and no goroutine is started.
func TestWarmupsNeedAFreeSlot(t *testing.T) {
	store, _ := newWritableStore(t)
	_, err := store.WriteVersion("/a.md", -1, []byte("# a\n"), nil)
	mustSucceed(t, err)
	for range cap(store.warmSlots) {
		store.warmSlots <- struct{}{}
	}
	if store.warm(context.Background(), "/a.md") != nil {
		t.Error("a warm-up started with every slot busy")
	}
	for range cap(store.warmSlots) {
		<-store.warmSlots
	}
}

// A closed store's reads still work when its snapshot went stale: the read
// reloads it itself, since no background work runs after Close.
func TestClosedStaleReplicaStillReads(t *testing.T) {
	objects := newClockedStore(t)
	objects.writtenAgo(slotRetention + time.Hour)
	stale := manualSite(objects).open(t, 0)
	checkpointedLog(t, manualSite(objects).open(t, 0))
	unconfirmed(stale)
	mustSucceed(t, stale.Close())
	if _, err := stale.Get("/log/016.md", 0); err != nil {
		t.Fatalf("read on a closed stale replica: %v", err)
	}
}
