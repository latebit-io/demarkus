package bucketstore

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// The compaction role's invariants (one compactor in steady state, a backstop
// steps within its delay, installs never wait on a rebase, a backstop holds
// no base) are in ADR 0036, amendment "a compaction role"; tests here hold them.

// roleSite opens stores of one role on objects, compaction manual and
// bounded to steps of 16, unhedged.
func roleSite(objects blob.Store, role CompactionRole) *bucketSite {
	return &bucketSite{objects: objects, trigger: stepping(16, 0), noHedge: true, role: role}
}

// With an eager store alive a backstop writes no checkpoint, the first after
// genesis included; alone, it steps once the newest checkpoint is older than
// its delay, whoever wrote it, and writes the backlog whole.
func TestBackstopDefersToAnEagerStore(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	eager := roleSite(objects, Eager).open(t, 0)
	backstop := roleSite(objects, Backstop).open(t, 0)
	writeDocuments(t, eager, 100)
	mustSucceed(t, backstop.poll(ctx))

	// Checkpoint zero exempts an eager store from the defer rule; a backstop
	// leaves the first step to the eager one.
	mustSucceed(t, backstop.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != 1 {
		t.Fatalf("backstop wrote checkpoint %d next to an eager store, want none after genesis", newest)
	}
	mustSucceed(t, eager.checkpoint(ctx))
	first := newestCheckpointOf(t, objects)
	if first != eager.servedSequence() {
		t.Fatalf("eager store checkpointed to %d of %d", first, eager.servedSequence())
	}

	// The backstop writes too; its runs adopt the eager store's checkpoint
	// and end, and keep no base of it.
	for index := range 50 {
		_, err := backstop.WriteVersion(fmt.Sprintf("/backstop/%02d.md", index), 0, fmt.Appendf(nil, "# %d\n", index), nil)
		mustSucceed(t, err)
	}
	for range 2 {
		mustSucceed(t, backstop.checkpoint(ctx))
	}
	if newest := newestCheckpointOf(t, objects); newest != first || backstop.layout().Sequence != first {
		t.Fatalf("newest checkpoint %d, backstop rests on %d; want both the eager store's %d", newest, backstop.layout().Sequence, first)
	}
	if backstop.compaction.base.Load() != nil {
		t.Error("backstop keeps a compaction base after adopting")
	}
	// Past the step bound an eager peer would take over; a backstop waits
	// out its delay, however old the checkpoint under it.
	backstop.now = func() time.Time { return time.Now().Add(2 * checkpointTimeout) }
	mustSucceed(t, backstop.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != first {
		t.Fatalf("backstop wrote checkpoint %d once the eager store's aged past the step bound, want none under the backstop delay", newest)
	}
	// Stored times are truncated to seconds, so age it past the delay by a margin.
	backstop.now = func() time.Time { return time.Now().Add(2 * backstopDelay) }
	mustSucceed(t, backstop.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != backstop.servedSequence() {
		t.Fatalf("backstop alone checkpointed to %d of %d once the newest checkpoint aged past its delay", newest, backstop.servedSequence())
	}
	if steps := checkpointSequences(t, objects, first); len(steps) != 1 {
		t.Errorf("backstop wrote checkpoints %v, want the backlog whole in one", steps)
	}
	if backstop.compaction.base.Load() != nil {
		t.Error("backstop keeps a compaction base after its own checkpoint")
	}
	readEveryVersion(t, backstop)
	live := worldDigest(t, backstop)
	if got := worldDigest(t, roleSite(objects, Eager).open(t, 0)); !reflect.DeepEqual(live, got) {
		t.Errorf("a backstop's checkpoint plus replay differ from the live snapshot:\nlive %+v\ncold %+v", live, got)
	}
}

// A backstop's own trigger runs its compactor like any store's; the run
// finds the eager store's checkpoint fresh and writes nothing.
func TestBackstopTriggerWritesNothingNextToAnEagerStore(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	eager := roleSite(objects, Eager).open(t, 0)
	quick := &compactionTrigger{slots: 4, age: time.Hour, wait: func() time.Duration { return 0 }}
	backstop := (&bucketSite{objects: objects, trigger: quick, noHedge: true, role: Backstop}).open(t, 0)
	for index := range 8 {
		_, err := backstop.WriteVersion(fmt.Sprintf("/doc/%d.md", index), 0, fmt.Appendf(nil, "# %d\n", index), nil)
		mustSucceed(t, err)
	}
	waitIdleCompactor(t, backstop)
	if newest := newestCheckpointOf(t, objects); newest != 1 {
		t.Fatalf("backstop's trigger wrote checkpoint %d, want none after genesis", newest)
	}
	mustSucceed(t, eager.poll(ctx))
	mustSucceed(t, eager.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != eager.servedSequence() {
		t.Fatalf("eager store checkpointed to %d of %d", newest, eager.servedSequence())
	}
}

// A backstop that adopts checkpoint after checkpoint keeps no base and no
// residue of the adoptions: outside what the bucket holds, heap stays under
// a couple of agent bodies over 40 more cycles of production-shaped writes.
func TestBackstopKeepsNoBaseAcrossAdoptions(t *testing.T) {
	ctx := context.Background()
	memory, err := blob.NewMemory(4 << 20)
	mustSucceed(t, err)
	objects := &storedBytes{Store: memory}
	mustSucceed(t, initialize(ctx, objects, testWorldID))
	eager := roleSite(objects, Eager).open(t, 0)
	backstop := roleSite(objects, Backstop).open(t, 0)
	cycle := func(n int) {
		publishPruned(t, eager, "/graph.md", memtest.AgentGraphBody(n))
		for server := range 5 {
			publishPruned(t, eager, fmt.Sprintf("/index/server-%d.md", server), fmt.Appendf(nil, "# Index %d\n\nCycle %d.\n", server, n))
		}
		mustSucceed(t, eager.checkpoint(ctx))
		mustSucceed(t, backstop.poll(ctx))
		mustSucceed(t, backstop.checkpoint(ctx))
		if backstop.layout().Sequence != eager.layout().Sequence {
			t.Fatalf("cycle %d: backstop rests on checkpoint %d, eager on %d", n, backstop.layout().Sequence, eager.layout().Sequence)
		}
		if backstop.compaction.base.Load() != nil {
			t.Fatalf("cycle %d: backstop keeps a compaction base", n)
		}
	}
	const warmup, cycles = 10, 40
	for n := range warmup {
		cycle(n)
	}
	waitIdle(t, eager)
	waitIdle(t, backstop)
	bytesBefore, objectsBefore := objects.bytes.Load(), objects.objects.Load()
	growth := memtest.Retained(func() {
		for n := warmup; n < warmup+cycles; n++ {
			cycle(n)
		}
		waitIdle(t, eager)
		waitIdle(t, backstop)
	})
	runtime.KeepAlive(eager)
	runtime.KeepAlive(backstop)
	if eager.compaction.base.Load() == nil {
		t.Error("eager store keeps no compaction base")
	}
	bucket := objects.bytes.Load() - bytesBefore + 512*(objects.objects.Load()-objectsBefore)
	bodyBytes := int64(len(memtest.AgentGraphBody(0)))
	t.Logf("heap grew %d bytes over %d adoptions, %d of them the bucket's", growth, cycles, bucket)
	if limit := 2 * bodyBytes; growth-bucket > limit {
		t.Errorf("heap outside the bucket grew %d bytes over %d adoptions, want under %d", growth-bucket, cycles, limit)
	}
}

// A commit confirms its slot while an adoption rebases the served snapshot:
// the rebase holds no lock an install needs, and the commit landing meanwhile
// restarts it on the new snapshot, which ends up both rebased and current.
func TestCommitConfirmsWhileAnAdoptionRebases(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	writer := manualSite(objects).open(t, 0)
	peer := (&bucketSite{objects: objects, trigger: manual, noHedge: true}).open(t, 0)
	writeDocuments(t, writer, 50)
	mustSucceed(t, peer.poll(ctx))
	mustSucceed(t, writer.checkpoint(ctx))
	checkpointed := writer.layout().Sequence
	before := recentVersions(peer)

	arrived, release := make(chan struct{}, 2), make(chan struct{})
	var rebases atomic.Int32
	peer.holdRebase = func() {
		first := rebases.Add(1) == 1
		arrived <- struct{}{}
		if first {
			<-release
		}
	}
	stepped := make(chan error, 1)
	go func() {
		_, err := peer.step(ctx)
		stepped <- err
	}()
	waitForTestSignal(t, arrived, "the adoption's rebase")
	written := publishAsync(ctx, peer, backend.WriteRequest{Path: "/d.md", ExpectedVersion: -1, Content: []byte("# d\n")})
	select {
	case outcome := <-written:
		mustSucceed(t, outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("the commit waited on an adoption's rebase")
	}
	committed := peer.servedSequence()
	close(release)
	mustSucceed(t, <-stepped)

	served := peer.served.Load().snap
	if served.Checkpoint.Sequence != checkpointed || served.Sequence != committed {
		t.Fatalf("served snapshot rests on checkpoint %d at sequence %d, want the adopted %d at the commit's %d", served.Checkpoint.Sequence, served.Sequence, checkpointed, committed)
	}
	if n := rebases.Load(); n != 2 {
		t.Errorf("the adoption rebased %d times, want a restart on the snapshot the commit installed", n)
	}
	if after := recentVersions(peer); after >= before {
		t.Errorf("served snapshot holds %d versions after the adoption, %d before; the checkpoint's were not dropped", after, before)
	}
	readEveryVersion(t, peer)
}

// The rebase a full step's adoption costs, for sizing the restart window
// against the commit cadence: logged, not asserted.
func TestAdoptionRebaseCost(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a full step of documents")
	}
	objects := initializedMemory(t)
	store := manualSite(objects).open(t, 0)
	writeDocuments(t, store, checkpointStep)
	served := store.served.Load().snap
	adopted, err := store.checkpointWriter().write(context.Background(), served)
	mustSucceed(t, err)
	if len(adopted.entries) != checkpointStep {
		t.Fatalf("adoption of %d entries, want %d", len(adopted.entries), checkpointStep)
	}
	started := time.Now()
	rebased := store.rebase(served, adopted)
	elapsed := time.Since(started)
	if rebased == served || store.diverged.Load() {
		t.Fatal("rebase failed")
	}
	t.Logf("rebasing %d entries onto %d documents took %v", len(adopted.entries), served.Paths.Len(), elapsed)
}
