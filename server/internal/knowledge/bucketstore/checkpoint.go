package bucketstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

const (
	// A store checkpoints once the newest checkpoint lags its snapshot by
	// checkpointSlots slots (a replay budget: spread writes rewrite every
	// shard, ADR 0036), or by checkpointAge since the first slot after it.
	checkpointSlots  = 8192
	checkpointAge    = 10 * time.Minute
	checkpointJitter = 10 * time.Second
	// checkpointTimeout bounds one compaction step or adoption, which no
	// request waits for.
	checkpointTimeout = 5 * time.Minute
	// backstopDelay is how old the newest checkpoint must be before a
	// backstop steps: an eager replica's trigger, its step, and one more.
	backstopDelay = checkpointAge + 2*checkpointTimeout
	// checkpointStep bounds the documents one checkpoint folds: a backlog past
	// it is checkpointed at earlier slot boundaries first, so every step ends
	// within checkpointTimeout and progress accumulates.
	checkpointStep = 1 << 15
	// The newest checkpointsKept stay, and any whose successor came within
	// the store's grace. A dropped one takes the shards, segments and root
	// only it used, each past the grace and at the generation read.
	checkpointsKept = 3
	// defaultCheckpointGrace is the grace when Options leaves it zero.
	defaultCheckpointGrace = minCheckpointGrace
	// minCheckpointGrace leaves a compactor that reused bytes just younger
	// than freshenAge time to finish (checkpointTimeout) before they can go.
	minCheckpointGrace = 2 * freshenAge
	// A slot the oldest kept checkpoint covers goes once slotRetention old, for
	// resumes and the deriver's cursors; a pruned blob too, for lagging readers.
	// A replica unconfirmed for staleAfter reloads before trusting a missing slot.
	slotRetention = 24 * time.Hour
	staleAfter    = slotRetention / 2
	// collectStep bounds the pruned blobs one step reads and deletes, and the
	// entries of one manifest part, so a step always finishes a part.
	collectStep = 512
)

// A drop must not take bytes a compactor reused just under freshenAge ago
// and is still writing, so the build fails if the grace cannot cover it.
const _ = uint64(minCheckpointGrace - freshenAge - checkpointTimeout)

// compactionTrigger is when a store's compactor runs; tests shorten it.
type compactionTrigger struct {
	slots int
	age   time.Duration
	wait  func() time.Duration
	// timeout, step, delay and collect override checkpointTimeout,
	// checkpointStep, backstopDelay and collectStep when set.
	timeout time.Duration
	step    int
	delay   time.Duration
	collect int
}

var defaultTrigger = compactionTrigger{
	slots: checkpointSlots,
	age:   checkpointAge,
	wait:  func() time.Duration { return jitterWithin(checkpointJitter) },
}

// compaction schedules a store's compactor: one run at a time, started by
// installed slots, stopped with the store.
type compaction struct {
	trigger compactionTrigger
	role    CompactionRole
	ctx     context.Context
	cancel  context.CancelFunc
	// base is the snapshot at the newest checkpoint, rebased on it, which a
	// bounded step replays from; it shares every unchanged node with the
	// served snapshot. Nil until a load or checkpoint gives one.
	base atomic.Pointer[snapshot]
	// written is the newest checkpoint this store wrote itself.
	written atomic.Int64

	mu      sync.Mutex
	slots   int       // installed since the newest checkpoint this store knows
	since   time.Time // when the first of them was installed
	running bool
	closed  bool
	done    sync.WaitGroup
}

func (c *compaction) timeout() time.Duration { return cmp.Or(c.trigger.timeout, checkpointTimeout) }

func (c *compaction) stepLimit() int { return cmp.Or(c.trigger.step, checkpointStep) }

func (c *compaction) backstopDelay() time.Duration { return cmp.Or(c.trigger.delay, backstopDelay) }

func (c *compaction) collectLimit() int { return cmp.Or(c.trigger.collect, collectStep) }

// keepsBase reports whether this store keeps a snapshot at the newest
// checkpoint to step from: an eager writer does, a backstop or a read-only
// store never steps from one.
func (store *Store) keepsBase() bool {
	return !store.readOnly && store.compaction.role == Eager
}

// noteSlots counts installed slots and starts the compactor once they lag
// enough. A world gone quiet keeps its last few slots until the next write.
func (store *Store) noteSlots(n int) {
	c := &store.compaction
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || n == 0 {
		return
	}
	if c.slots == 0 {
		c.since = store.now()
	}
	c.slots += n
	if c.running || c.slots < c.trigger.slots && store.now().Sub(c.since) < c.trigger.age {
		return
	}
	c.running = true
	c.done.Add(1)
	go store.compact(c.slots)
}

// compact runs the compactor after its wait, a step at a time until the
// served snapshot is checkpointed or a step fails, and forgets the slots it
// covered: those counted when it started.
func (store *Store) compact(counted int) {
	c := &store.compaction
	err := waitForRetry(c.ctx, c.trigger.wait())
	for more := err == nil; more; {
		ctx, cancel := context.WithTimeout(c.ctx, c.timeout())
		more, err = store.step(ctx)
		cancel()
	}
	if err != nil && c.ctx.Err() == nil {
		store.logger.Warn("checkpoint failed; replay grows until one succeeds", "world", store.worldID, "error", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	c.slots = max(c.slots-counted, 0)
	c.since = store.now()
	c.done.Done()
}

// stopCompaction ends a run in progress and refuses new ones.
func (store *Store) stopCompaction() {
	c := &store.compaction
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.done.Wait()
}

// checkpoint brings the store's newest checkpoint up to its snapshot, in as
// many steps as it takes.
func (store *Store) checkpoint(ctx context.Context) error {
	for {
		more, err := store.step(ctx)
		if err != nil || !more {
			return err
		}
	}
}

// step adopts a peer's newer checkpoint, ending the run since that peer is
// compacting, or writes one of the served snapshot, or of an earlier slot
// boundary when its changes exceed a step; more while served still lags.
func (store *Store) step(ctx context.Context) (bool, error) {
	// Only checkpoints after the known one are listed: the rest may be many
	// until they are collected.
	known := store.layout().Sequence
	newer, err := listCheckpoints(ctx, store.objects, known)
	if err != nil {
		return false, err
	}
	adopted, broken, err := store.adoptNewer(ctx, newer)
	if adopted || err != nil {
		return false, err
	}
	// A broken checkpoint still holds its name, so the next is written past it.
	newest := lastOr(newer, known)
	snap := store.served.Load().snap
	if store.readOnly || snap.Sequence <= newest {
		return false, nil
	}
	// A snapshot that failed to rebase on the newest checkpoint would name
	// shards of an older one, which a drop may already have taken.
	if snap.Checkpoint.Sequence != known {
		return false, fmt.Errorf("%w: snapshot rests on checkpoint %d, not the newest %d", blob.ErrIntegrity, snap.Checkpoint.Sequence, known)
	}
	if deferred, err := store.deferToPeer(ctx, known); deferred || err != nil {
		return false, err
	}
	target, err := store.stepTarget(ctx, snap, broken)
	if err != nil {
		return false, err
	}
	written, err := store.checkpointWriter().write(ctx, target)
	if err != nil {
		return false, err
	}
	store.installAdoption(written)
	if written.own {
		store.compaction.written.Store(target.Sequence)
	}
	store.keepBase(target, written)
	store.logger.Info("checkpoint written", "world", store.worldID, "sequence", target.Sequence, "served", snap.Sequence)
	plan, err := store.planDrop(ctx)
	if err == nil {
		err = errors.Join(store.applyDrop(ctx, plan), store.dropSlots(ctx, plan))
	}
	// A checkpoint a failed drop left behind may still retain pruned versions.
	if err == nil {
		store.collectBlobs(ctx, plan)
	}
	if err != nil {
		store.logger.Warn("old checkpoints or slots not dropped; a later checkpoint retries", "world", store.worldID, "error", err)
	}
	return target != snap, nil
}

// adoptNewer adopts the newest of the newer checkpoints that loads, skipping
// one missing or corrupt in any object it names; it returns those skipped.
// A bucket or caller failure ends it, as no reason to skip.
func (store *Store) adoptNewer(ctx context.Context, newer []int64) (adopted bool, broken []int64, err error) {
	for _, sequence := range slices.Backward(newer) {
		if err = store.adoptPeerCheckpoint(ctx, sequence); err == nil {
			return true, broken, nil
		}
		if !damaged(ctx, err) {
			return false, broken, err
		}
		store.logger.Warn("newer checkpoint does not load; skipping it", "world", store.worldID, "checkpoint", sequence, "error", err)
		broken = append(broken, sequence)
	}
	return false, broken, nil
}

// deferToPeer reports whether the newest checkpoint is one to leave alone:
// for an eager store a peer's within one step bound (that peer is compacting;
// zero and its own never defer), for a backstop any one within its delay.
func (store *Store) deferToPeer(ctx context.Context, known int64) (bool, error) {
	c := &store.compaction
	hold := c.timeout()
	if c.role == Backstop {
		hold = c.backstopDelay()
	} else if known <= 1 || known == c.written.Load() {
		return false, nil
	}
	attributes, err := store.objects.Head(ctx, checkpointKey(known))
	if err != nil {
		return false, fmt.Errorf("head checkpoint %d: %w", known, err)
	}
	return store.now().Sub(attributes.Modified) < hold, nil
}

// stepTarget is the snapshot the next checkpoint covers: snap when its
// changes fit one step, else the base replayed to the slot boundary that
// fills a step, never a broken checkpoint's; without a base, the whole backlog.
func (store *Store) stepTarget(ctx context.Context, snap *snapshot, broken []int64) (*snapshot, error) {
	c := &store.compaction
	limit := c.stepLimit()
	if changedDocuments(snap, limit) <= limit {
		return snap, nil
	}
	base := c.base.Load()
	if base == nil || base.Checkpoint.Sequence != snap.Checkpoint.Sequence {
		store.logger.Warn("no snapshot at the newest checkpoint; checkpointing the whole backlog", "world", store.worldID, "checkpoint", snap.Checkpoint.Sequence)
		return snap, nil
	}
	target := store.derive(base)
	touched := make(map[string]struct{}, limit)
	options := replayOptions{
		worldID: store.worldID, workers: store.shardWorkers, until: snap.Sequence,
		stop: func(slot *slotObject) bool {
			for index := range slot.Entries {
				touched[slot.Entries[index].Path] = struct{}{}
			}
			// Steps are deterministic, so ending where a broken one did would
			// meet its bytes under create-if-absent on every run.
			return len(touched) >= limit && !slices.Contains(broken, slot.last())
		},
	}
	if err := replay(ctx, store.objects, target, options); err != nil {
		return nil, fmt.Errorf("checkpoint step from %d: %w", base.Sequence, err)
	}
	if target.Sequence >= snap.Sequence {
		return snap, nil
	}
	return target, nil
}

// changedDocuments counts the documents changed since snap's checkpoint, up
// to limit+1.
func changedDocuments(snap *snapshot, limit int) int {
	count := 0
	snap.Paths.Ascend(func(state *pathState) bool {
		if !state.unchanged() {
			count++
		}
		return count <= limit
	})
	return count
}

// keepBase keeps target, rebased on the checkpoint just written of it, as
// the base the next step replays from; a backstop keeps none.
func (store *Store) keepBase(target *snapshot, adopted *adoption) {
	if !store.keepsBase() {
		return
	}
	next := store.derive(target)
	if err := next.adopt(adopted); err != nil {
		store.logger.Error("snapshot at the new checkpoint failed to rebase; the next step writes the whole backlog", "world", store.worldID, "checkpoint", adopted.checkpoint.Sequence, "error", err)
		store.compaction.base.Store(nil)
		return
	}
	store.compaction.base.Store(next)
}

// advanceBase replays the base to a peer's checkpoint and rebases it there,
// so this store can step from it too. A base that cannot follow is dropped;
// the next checkpoint this store writes sets one again.
func (store *Store) advanceBase(ctx context.Context, from int64, a *adoption) {
	c := &store.compaction
	base := c.base.Load()
	if !store.keepsBase() || base == nil {
		return
	}
	if base.Checkpoint.Sequence != from {
		c.base.Store(nil)
		return
	}
	next := store.derive(base)
	err := replay(ctx, store.objects, next, replayOptions{worldID: store.worldID, workers: store.shardWorkers, until: a.checkpoint.Sequence})
	if err == nil && next.Sequence != a.checkpoint.Sequence {
		err = fmt.Errorf("%w: checkpoint %d is not a slot boundary; replay reached %d", blob.ErrIntegrity, a.checkpoint.Sequence, next.Sequence)
	}
	if err == nil {
		err = next.adopt(a)
	}
	if err != nil {
		store.logger.Warn("snapshot at the peer's checkpoint not built; the next own checkpoint sets one", "world", store.worldID, "checkpoint", a.checkpoint.Sequence, "error", err)
		c.base.Store(nil)
		return
	}
	c.base.Store(next)
}
