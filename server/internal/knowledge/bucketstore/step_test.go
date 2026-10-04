package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/blob"
)

// stepping is a manual trigger whose steps hold at most step documents and
// end within timeout.
func stepping(step int, timeout time.Duration) *compactionTrigger {
	trigger := *manual
	trigger.step, trigger.timeout = step, timeout
	return &trigger
}

// startCompactor runs the compactor now, as noteSlots does once slots lag.
func startCompactor(t *testing.T, store *Store) {
	t.Helper()
	c := &store.compaction
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running || c.closed {
		t.Fatal("compactor already running or closed")
	}
	c.running = true
	c.done.Add(1)
	go store.compact(0)
}

func newestCheckpointOf(t *testing.T, objects blob.Store) int64 {
	t.Helper()
	sequence, err := newestCheckpointSequence(context.Background(), objects, 0)
	mustSucceed(t, err)
	return sequence
}

// recentVersions counts the versions the served snapshot holds in memory.
func recentVersions(store *Store) int {
	n := 0
	store.served.Load().snap.Paths.Ascend(func(state *pathState) bool {
		n += len(state.Recent)
		return true
	})
	return n
}

// A backlog too large to checkpoint within the timeout never converges when
// each run writes it whole: the retry starts over. In bounded steps every
// run ends within the timeout and the checkpoint reaches the snapshot.
func TestCompactionConvergesInBoundedSteps(t *testing.T) {
	const documents, latency, timeout = 300, 2 * time.Millisecond, 400 * time.Millisecond
	backlog := func(t *testing.T, trigger *compactionTrigger) (*Store, blob.Store) {
		t.Helper()
		objects := latentStore{Store: initializedMemory(t), delay: latency}
		store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ShardWorkers: 1, noHedge: true, trigger: trigger})
		mustSucceed(t, err)
		closeAtEnd(t, store)
		writeDocuments(t, store, documents)
		waitIdle(t, store)
		return store, objects
	}

	t.Run("whole backlog never lands", func(t *testing.T) {
		store, objects := backlog(t, stepping(math.MaxInt, timeout))
		for attempt := range 2 {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			_, err := store.step(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("attempt %d: step returned %v, want the deadline exceeded", attempt, err)
			}
			if newest := newestCheckpointOf(t, objects); newest != 1 {
				t.Fatalf("attempt %d: checkpoint %d landed within %s", attempt, newest, timeout)
			}
		}
	})

	t.Run("steps converge", func(t *testing.T) {
		store, objects := backlog(t, stepping(16, timeout))
		served := store.servedSequence()
		startCompactor(t, store)
		deadline := time.Now().Add(30 * time.Second)
		for newestCheckpointOf(t, objects) < served {
			if time.Now().After(deadline) {
				t.Fatalf("checkpoint at %d after 30 s, served %d", newestCheckpointOf(t, objects), served)
			}
			time.Sleep(10 * time.Millisecond)
		}
		waitIdleCompactor(t, store)
		if steps := checkpointSequences(t, objects, 1); len(steps) < 3 {
			t.Errorf("checkpoints %v, want several bounded steps", steps)
		}
		if n := recentVersions(store); n != 0 {
			t.Errorf("served snapshot holds %d versions the checkpoints have", n)
		}
		live := worldDigest(t, store)
		cold := (&bucketSite{objects: objects, trigger: manual, noHedge: true}).open(t, 0)
		if got := worldDigest(t, cold); !reflect.DeepEqual(live, got) {
			t.Errorf("stepped checkpoints plus replay differ from the live snapshot:\nlive %+v\ncold %+v", live, got)
		}
	})
}

// Two replicas with the same backlog cut a step at the same slot boundary,
// so their racing steps write identical bytes.
func TestStepBoundaryIsDeterministic(t *testing.T) {
	ctx := context.Background()
	site := &bucketSite{objects: initializedMemory(t), trigger: stepping(16, 0), noHedge: true}
	writer, peer := site.open(t, 0), site.open(t, 0)
	writeDocuments(t, writer, 200)
	mustSucceed(t, peer.poll(ctx))
	var targets [2]*snapshot
	for index, store := range []*Store{writer, peer} {
		target, err := store.stepTarget(ctx, store.served.Load().snap)
		mustSucceed(t, err)
		targets[index] = target
	}
	served := writer.servedSequence()
	if targets[0].Sequence != targets[1].Sequence || targets[0].Sequence <= 1 || targets[0].Sequence >= served {
		t.Fatalf("step targets at %d and %d, want one bounded sequence between 1 and %d", targets[0].Sequence, targets[1].Sequence, served)
	}
	if got, want := snapshotDigest(targets[0]), snapshotDigest(targets[1]); !reflect.DeepEqual(got, want) {
		t.Error("step targets at one sequence differ")
	}
	// Racing on one step, exactly one compactor's create makes the
	// checkpoint; the other learns it was a racer's and defers from then on.
	var wg sync.WaitGroup
	results := make([]*adoption, 2)
	for index, target := range targets {
		wg.Go(func() {
			adopted, err := testCheckpointWriter(site.objects).write(ctx, target)
			if err != nil {
				t.Errorf("compactor %d: %v", index, err)
			}
			results[index] = adopted
		})
	}
	wg.Wait()
	if results[0] == nil || results[1] == nil || results[0].own == results[1].own {
		t.Errorf("racing steps report own = %v, %v; want exactly one", results[0] != nil && results[0].own, results[1] != nil && results[1].own)
	}
}

// A replica that adopts a peer's step keeps a snapshot at it, so when the
// peer stops it steps on from there instead of writing the whole backlog.
func TestAdopterStepsFromItsOwnBase(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	// A step bound of a nanosecond, so the peer never defers to the writer.
	site := &bucketSite{objects: objects, trigger: stepping(16, time.Nanosecond), noHedge: true}
	writer, peer := site.open(t, 0), site.open(t, 0)
	writeDocuments(t, writer, 200)
	mustSucceed(t, peer.poll(ctx))
	served := writer.servedSequence()
	before := recentVersions(writer)

	more, err := writer.step(ctx)
	mustSucceed(t, err)
	first := writer.layout().Sequence
	if !more || first <= 1 || first >= served {
		t.Fatalf("first step wrote checkpoint %d (more %t), want a bounded one below %d", first, more, served)
	}
	if after := recentVersions(writer); after >= before {
		t.Errorf("served snapshot holds %d versions after the step, %d before", after, before)
	}

	more, err = peer.step(ctx)
	mustSucceed(t, err)
	if more || peer.layout().Sequence != first {
		t.Fatalf("peer rests on checkpoint %d (more %t), want to adopt %d and end its run", peer.layout().Sequence, more, first)
	}
	_, err = peer.step(ctx)
	mustSucceed(t, err)
	second := peer.layout().Sequence
	if second <= first || second >= served {
		t.Fatalf("peer wrote checkpoint %d after adopting %d, want a bounded step below %d", second, first, served)
	}

	// The two take turns until the backlog is checkpointed, each from the
	// base it kept of the other's step.
	for sequence := second; sequence < served; {
		for _, store := range []*Store{writer, peer} {
			mustSucceed(t, store.checkpoint(ctx))
		}
		if next := newestCheckpointOf(t, objects); next <= sequence {
			t.Fatalf("no progress past checkpoint %d", sequence)
		} else {
			sequence = next
		}
	}
	live := worldDigest(t, writer)
	if got := worldDigest(t, site.open(t, 0)); !reflect.DeepEqual(live, got) {
		t.Errorf("alternating steps plus replay differ from the live snapshot:\nlive %+v\ncold %+v", live, got)
	}
}

// A replica whose newest checkpoint is a peer's, written within one step
// bound, adopts and writes nothing: that peer is compacting. Once the
// checkpoint is older than the bound, or when it is its own, it steps.
func TestFollowerDefersToAFreshCompactor(t *testing.T) {
	ctx := context.Background()
	objects := initializedMemory(t)
	site := &bucketSite{objects: objects, trigger: stepping(16, 0), noHedge: true}
	writer, peer := site.open(t, 0), site.open(t, 0)
	writeDocuments(t, writer, 200)
	mustSucceed(t, peer.poll(ctx))
	more, err := writer.step(ctx)
	mustSucceed(t, err)
	first := writer.layout().Sequence
	if !more {
		t.Fatalf("writer's first step at %d left nothing to do", first)
	}
	// The peer's first run adopts the writer's step and ends; its next run
	// finds nothing newer and defers to the writer's fresh checkpoint.
	for range 2 {
		mustSucceed(t, peer.checkpoint(ctx))
	}
	if peer.layout().Sequence != first || newestCheckpointOf(t, objects) != first {
		t.Fatalf("peer rests on %d, newest %d; want it to adopt %d and write nothing while the writer's checkpoint is fresh", peer.layout().Sequence, newestCheckpointOf(t, objects), first)
	}
	mustSucceed(t, writer.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != writer.servedSequence() {
		t.Fatalf("the writer, whose own checkpoint it was, stopped at %d of %d", newest, writer.servedSequence())
	}
	// The writer stops; once its checkpoint has aged past the bound, the peer
	// takes over the next backlog.
	for index := range 100 {
		_, err := peer.WriteVersion(fmt.Sprintf("/more/%04d.md", index), 0, fmt.Appendf(nil, "# %d\n", index), nil)
		mustSucceed(t, err)
	}
	// Stored times are truncated to seconds, so age it past the bound by a margin.
	peer.now = func() time.Time { return time.Now().Add(2 * checkpointTimeout) }
	// The first run adopts the writer's last checkpoint and ends; the next steps.
	mustSucceed(t, peer.checkpoint(ctx))
	mustSucceed(t, peer.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != peer.servedSequence() {
		t.Errorf("peer wrote up to %d of %d after the writer's checkpoint aged", newest, peer.servedSequence())
	}
}

// preemptedCheckpoints has a racer create every checkpoint object first
// while on, so the store's own create finds it taken.
type preemptedCheckpoints struct {
	blob.Store
	on atomic.Bool
}

func (s *preemptedCheckpoints) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if s.on.Load() && strings.HasPrefix(key, checkpointPrefix) {
		if _, err := s.Store.Create(ctx, key, data); err != nil {
			return blob.Attributes{}, err
		}
	}
	return s.Store.Create(ctx, key, data)
}

// A step whose checkpoint a racer created first is the racer's: the store
// defers to it as to any peer's fresh checkpoint instead of stepping on.
func TestRacedCheckpointIsNotOwn(t *testing.T) {
	ctx := context.Background()
	objects := &preemptedCheckpoints{Store: initializedMemory(t)}
	store := (&bucketSite{objects: objects, trigger: stepping(16, 0), noHedge: true}).open(t, 0)
	writeDocuments(t, store, 200)
	objects.on.Store(true)
	more, err := store.step(ctx)
	mustSucceed(t, err)
	first := store.layout().Sequence
	if !more || first <= 1 || store.compaction.written.Load() != 0 {
		t.Fatalf("raced step at %d (more %t) counts as own: written %d", first, more, store.compaction.written.Load())
	}
	objects.on.Store(false)
	more, err = store.step(ctx)
	mustSucceed(t, err)
	if more || newestCheckpointOf(t, objects) != first {
		t.Fatalf("after a raced step the store stepped on to %d (more %t), want it to defer to the racer's fresh checkpoint %d", newestCheckpointOf(t, objects), more, first)
	}
	store.now = func() time.Time { return time.Now().Add(2 * checkpointTimeout) }
	mustSucceed(t, store.checkpoint(ctx))
	if newest := newestCheckpointOf(t, objects); newest != store.servedSequence() {
		t.Errorf("newest checkpoint %d of %d once the racer's aged", newest, store.servedSequence())
	}
}

// callCounter counts bucket calls by operation and key.
type callCounter struct {
	blob.Store
	mu     sync.Mutex
	counts map[string]int
}

func (s *callCounter) note(op, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = make(map[string]int)
	}
	s.counts[op+" "+key]++
}

func (s *callCounter) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = nil
}

// count is how many calls of op touched keys under prefix.
func (s *callCounter) count(op, prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for call, count := range s.counts {
		if strings.HasPrefix(call, op+" "+prefix) {
			n += count
		}
	}
	return n
}

func (s *callCounter) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	s.note("create", key)
	return s.Store.Create(ctx, key, data)
}

func (s *callCounter) Get(ctx context.Context, key string) (blob.Object, error) {
	s.note("get", key)
	return s.Store.Get(ctx, key)
}

func (s *callCounter) Head(ctx context.Context, key string) (blob.Attributes, error) {
	s.note("head", key)
	return s.Store.Head(ctx, key)
}

func (s *callCounter) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	s.note("replace", key)
	return s.Store.Replace(ctx, key, generation, data)
}

// A history block or blob that is already there costs its create alone:
// nothing collects them, so a taken content-addressed name is never read
// back or rewritten, however old the stored bytes.
func TestContentCreatesSkipReadBackAndFreshen(t *testing.T) {
	ctx := context.Background()
	clock := newClockedStore(t)
	clock.writtenAgo(2 * time.Hour)
	objects := &callCounter{Store: clock}
	site := &bucketSite{objects: objects, trigger: manual, noHedge: true}

	t.Run("history blocks", func(t *testing.T) {
		writer, peer := site.open(t, 0), site.open(t, 0)
		writeWorld(t, writer, 0)
		mustSucceed(t, writer.checkpoint(ctx))
		writeWorld(t, peer, 1)
		mustSucceed(t, writer.poll(ctx))
		mustSucceed(t, peer.poll(ctx))
		_, err := testCheckpointWriter(objects).write(ctx, writer.served.Load().snap)
		mustSucceed(t, err)
		objects.reset()
		// The peer rests on checkpoint zero: every block it folds is one the
		// writer created, and none extends a block it would have to read.
		_, err = testCheckpointWriter(objects).write(ctx, peer.served.Load().snap)
		mustSucceed(t, err)
		prefix := objectPrefix + "history/"
		if creates := objects.count("create", prefix); creates == 0 {
			t.Fatal("the peer's checkpoint created no history block")
		}
		if gets, replaces := objects.count("get", prefix), objects.count("replace", prefix); gets != 0 || replaces != 0 {
			t.Errorf("history blocks already there cost %d reads and %d rewrites, want none", gets, replaces)
		}
	})

	t.Run("blobs", func(t *testing.T) {
		writer := site.open(t, 0)
		body := []byte("# Same body\n\nstored once\n")
		_, err := writer.WriteVersion("/same/a.md", 0, body, nil)
		mustSucceed(t, err)
		key := writer.served.Load().snap.path("/same/a.md").Recent[0].entry.Blob.Key
		objects.reset()
		_, err = writer.WriteVersion("/same/b.md", 0, body, nil)
		mustSucceed(t, err)
		if creates := objects.count("create", key); creates != 1 {
			t.Fatalf("the second write of one body created its blob %d times, want once", creates)
		}
		if gets, replaces := objects.count("get", key), objects.count("replace", key); gets != 0 || replaces != 0 {
			t.Errorf("a blob already there cost %d reads and %d rewrites, want none", gets, replaces)
		}
	})
}

// ambiguousOnce answers the first create ambiguously, having stored the
// object or not.
type ambiguousOnce struct {
	blob.Store
	stored  bool
	once    sync.Once
	creates int
}

func (s *ambiguousOnce) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	s.creates++
	ambiguous := false
	s.once.Do(func() { ambiguous = true })
	if !ambiguous {
		return s.Store.Create(ctx, key, data)
	}
	if s.stored {
		if _, err := s.Store.Create(ctx, key, data); err != nil {
			return blob.Attributes{}, err
		}
	}
	return blob.Attributes{}, fmt.Errorf("%w: %w", blob.ErrAmbiguous, blob.ErrUnavailable)
}

// An ambiguous content create is settled by a metadata read: the object
// there is this content, a missing one is created again.
func TestCreateContentReconcilesAmbiguity(t *testing.T) {
	for _, stored := range []bool{true, false} {
		t.Run(fmt.Sprintf("stored=%t", stored), func(t *testing.T) {
			objects := &ambiguousOnce{Store: newTestMemory(t), stored: stored}
			object := modelObject{Key: blobKey("abc"), Data: []byte("content")}
			mustSucceed(t, createContent(context.Background(), objects, object))
			if got := getObject(t, objects, object.Key); string(got.Data) != "content" {
				t.Fatalf("stored %q", got.Data)
			}
			want := 2
			if stored {
				want = 1
			}
			if objects.creates != want {
				t.Errorf("%d creates, want %d", objects.creates, want)
			}
		})
	}
}

// Checkpointing a backlog in steps leaves the store holding what one whole
// checkpoint would: the snapshot at the newest checkpoint, and no earlier
// base or intermediate snapshot.
func TestSteppedCheckpointsLeaveNoResidue(t *testing.T) {
	const documents = 640
	owned := func(step int) int64 {
		objects := initializedMemory(t)
		defer runtime.KeepAlive(objects)
		return memtest.Owned(func() any {
			store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, trigger: stepping(step, 0), noHedge: true})
			mustSucceed(t, err)
			var wg sync.WaitGroup
			for n := range documents {
				wg.Go(func() {
					path := fmt.Sprintf("/agents/session-%d/inbox/01HZX%08d.md", n%7, n)
					body := fmt.Appendf(nil, "# Observation %d\n\n%s observed_at=2026-10-04T00:00:%02d.%09dZ\n", n, strings.Repeat("x", 200), n%60, n)
					if _, err := store.WriteVersion(path, 0, body, map[string]string{"agent": fmt.Sprintf("federation-agent-%d", n%3)}); err != nil {
						t.Errorf("write %d: %v", n, err)
					}
				})
			}
			wg.Wait()
			mustSucceed(t, store.checkpoint(context.Background()))
			waitIdle(t, store)
			mustSucceed(t, store.Close())
			return store
		})
	}
	whole, stepped := owned(math.MaxInt), owned(32)
	t.Logf("store owns %d bytes after one checkpoint, %d after steps of 32", whole, stepped)
	if stepped-whole > 128<<10 {
		t.Errorf("steps left %d bytes behind, want under 128 KiB: an intermediate snapshot or base is retained", stepped-whole)
	}
}
