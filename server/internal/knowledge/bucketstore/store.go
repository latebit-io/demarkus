package bucketstore

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

const (
	defaultRequestTimeout = 10 * time.Second
	defaultShardWorkers   = 16
)

// CompactionRole is a replica's part in checkpointing one world, set by the
// server: one eager replica per world, the rest backstops.
type CompactionRole int

const (
	// Eager checkpoints on the cadence (8,192 slots or 10 minutes), defers
	// to a peer's checkpoint fresher than one step bound, never to its own,
	// and keeps a base to step from. The default.
	Eager CompactionRole = iota
	// Backstop checkpoints only when the newest checkpoint, whoever wrote
	// it, is older than the cadence plus two step bounds, so a live eager
	// replica always moves first; it keeps no base and writes the backlog whole.
	Backstop
)

// Options configures one world store.
type Options struct {
	WorldID        string
	RequestTimeout time.Duration
	ShardWorkers   int
	// Compaction is this replica's role in checkpointing; zero is Eager.
	Compaction CompactionRole
	// ReadOnly makes every write answer backend.ErrReadOnly before any I/O,
	// and Open fails on an empty bucket instead of creating the world.
	ReadOnly bool
	// MaxDocuments caps distinct document paths (0 = unlimited); a new
	// path beyond the cap is rejected. Approximate under concurrency:
	// a per-tenant quota, not an exact invariant.
	MaxDocuments int
	// Logger receives the store's warnings and errors; required.
	Logger *slog.Logger
	// ChangeRing enables WATCH: the hub keeps this many events from the
	// applied slots, under the log's sequence, and the store follows peers
	// until Close. Zero leaves WATCH off.
	ChangeRing int
	// Committed runs with each sequence this replica learns the log reached,
	// by its own slot or one it lost, in order on its own goroutine; close
	// ones may be reported once. Nil for none.
	Committed func(sequence int64)
	// CheckpointGrace is how long a superseded checkpoint and what only it
	// uses outlive their successor: 0 is defaultCheckpointGrace, and less than
	// minCheckpointGrace is refused.
	CheckpointGrace time.Duration

	// followInterval overrides the backstop poll's period in tests.
	followInterval time.Duration
	// noHedge keeps the bucket unhedged, for tests that hold one call.
	noHedge bool
	// trigger overrides when the compactor runs, in tests.
	trigger *compactionTrigger
	// shardBudget shrinks maxShardBytes, in tests.
	shardBudget int
}

// Store serves one world from the log: the snapshot it last applied, and
// slots it creates to commit.
type Store struct {
	objects        blob.Store
	worldID        string
	id             string // names this store in the slots it writes
	requestTimeout time.Duration
	shardWorkers   int
	// checkpointGrace is Options.CheckpointGrace, defaulted.
	checkpointGrace time.Duration
	shardBudget     int
	logger          *slog.Logger
	maxDocuments    int
	readOnly        bool
	served          atomic.Pointer[served]
	// probing is the probe of the log's tip in flight, which refreshes share
	// (one bucket reader at a time); nil when none runs.
	probeMu sync.Mutex
	probing *probeFlight
	// installMu orders installs and is never held across a bucket call, so a
	// commit confirming its slot never waits on a probe.
	installMu sync.Mutex
	// deriveMu serializes snapshot clones: btree.Clone writes to its source.
	deriveMu sync.Mutex
	commits  commitQueue
	now      func() time.Time
	// own are the sequence ranges of this store's newest slots: a peer's hint
	// inside one means the peer lost that slot, and waiting records it.
	// peerSlots counts installed slots other stores wrote.
	ownMu       sync.Mutex
	own         [ownSlots]sequenceRange
	ownNext     int
	waiting     atomic.Pointer[peerWaiting]
	peerSlots   atomic.Int64
	handoffWait time.Duration
	hold        handoff // the committer's alone
	// pending is the slot being created and the snapshot it makes.
	pending atomic.Pointer[pendingSlot]
	// adoption is the newest checkpoint this store rebased on; a snapshot
	// built before it rebases as it is installed.
	adoption   atomic.Pointer[adoption]
	compaction compaction
	// holdRebase, when set, runs before an adoption rebases the served
	// snapshot outside installMu; tests hold it there.
	holdRebase func()
	sections   *sectionIndex
	// newestBatch is the committer's newest batch's objects, which the next
	// batch and warm-ups read before the bucket; nil while it is idle.
	newestBatch atomic.Pointer[batchObjects]
	// warming holds the warm-ups still reading, by path; warmSlots bounds them.
	warmMu    sync.Mutex
	warming   map[string]*warmup
	warmSlots chan struct{}

	changes   *changefeed.Hub
	changeLog *changeLog // the hub's backlog; nil without WATCH
	committed func(sequence int64)
	follower  *follower // nil without WATCH
	// hints carries the newest created sequence to the Committed goroutine;
	// nil without the hook.
	hints  chan int64
	hinted chan struct{}

	// life ends at Close; work started through background stops with it,
	// and Close waits for it.
	lifeMu     sync.Mutex
	life       context.Context
	end        context.CancelFunc
	background sync.WaitGroup
	// reloading is the reload stale refreshes share; nil when none runs.
	// diverged is set when a snapshot failed to rebase on a checkpoint.
	reloadMu  sync.Mutex
	reloading *reloadFlight
	diverged  atomic.Bool
	hintsOnce sync.Once
}

var (
	_ backend.Store        = (*Store)(nil)
	_ backend.ViewProvider = (*Store)(nil)
	_ backend.ChangeSource = (*Store)(nil)
	_ backend.Follower     = (*Store)(nil)
)

// Open loads the world, creating it in an empty bucket unless read-only.
func Open(ctx context.Context, objects blob.Store, options Options) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open bucket store: %w: context is nil", blob.ErrPrecondition)
	}
	if options.Logger == nil {
		return nil, fmt.Errorf("open bucket store: %w: logger is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return nil, fmt.Errorf("open bucket store: %w: blob store is nil", blob.ErrPrecondition)
	}
	if !validUUID(options.WorldID) {
		return nil, fmt.Errorf("open bucket store: %w: invalid world ID %q", blob.ErrPrecondition, options.WorldID)
	}
	if options.RequestTimeout < 0 {
		return nil, fmt.Errorf("open bucket store: %w: request timeout must not be negative", blob.ErrPrecondition)
	}
	if options.ShardWorkers < 0 {
		return nil, fmt.Errorf("open bucket store: %w: shard workers must not be negative", blob.ErrPrecondition)
	}
	if options.MaxDocuments < 0 {
		return nil, fmt.Errorf("open bucket store: %w: max documents must not be negative", blob.ErrPrecondition)
	}
	if options.Compaction != Eager && options.Compaction != Backstop {
		return nil, fmt.Errorf("open bucket store: %w: unknown compaction role %d", blob.ErrPrecondition, options.Compaction)
	}
	if options.CheckpointGrace == 0 {
		options.CheckpointGrace = defaultCheckpointGrace
	}
	if options.CheckpointGrace < minCheckpointGrace {
		return nil, fmt.Errorf("open bucket store: %w: checkpoint grace %s is under the minimum %s", blob.ErrPrecondition, options.CheckpointGrace, minCheckpointGrace)
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.ShardWorkers == 0 {
		options.ShardWorkers = defaultShardWorkers
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	id, err := randomOperationID()
	if err != nil {
		return nil, fmt.Errorf("open bucket store: store ID: %w", err)
	}

	if !options.noHedge {
		// Creates reconcile by reading the name back, which a hedge needs.
		objects = blob.Hedged(objects)
	}
	store := &Store{
		objects:         objects,
		worldID:         options.WorldID,
		id:              id,
		requestTimeout:  options.RequestTimeout,
		checkpointGrace: options.CheckpointGrace,
		shardBudget:     cmp.Or(options.shardBudget, maxShardBytes),
		shardWorkers:    options.ShardWorkers,
		logger:          options.Logger,
		maxDocuments:    options.MaxDocuments,
		readOnly:        options.ReadOnly,
		commits:         commitQueue{wake: make(chan struct{}, 1)},
		now:             time.Now,
		handoffWait:     handoffWait,
		warming:         make(map[string]*warmup),
		warmSlots:       make(chan struct{}, options.ShardWorkers),
		committed:       options.Committed,
	}
	store.compaction.trigger = defaultTrigger
	store.compaction.role = options.Compaction
	if options.trigger != nil {
		store.compaction.trigger = *options.trigger
	}
	store.compaction.ctx, store.compaction.cancel = context.WithCancel(context.Background())
	store.life, store.end = context.WithCancel(context.Background())
	store.sections = newSectionIndex(store)
	if options.ChangeRing > 0 {
		store.changeLog = newChangeLog(store, options.ChangeRing)
		store.changes = changefeed.NewWithBacklog(store.worldID, options.ChangeRing, store.changeLog)
	}
	if err := store.loadOrCreate(ctx); err != nil {
		store.compaction.cancel()
		store.end()
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	if store.changes != nil {
		store.startFollowing(cmp.Or(options.followInterval, followInterval))
	}
	if store.committed != nil {
		store.hints, store.hinted = make(chan int64, 1), make(chan struct{})
		go store.notify()
	}
	return store, nil
}

// WarmSections starts the section index now rather than on the first body
// search, so a write gate judges the first writes after open; nothing on a
// read-only store, whose gate never runs.
func (store *Store) WarmSections() {
	if !store.readOnly {
		store.sections.activate()
	}
}

// notify runs the Committed hook, apart from the committer, so a hook that
// writes to this store cannot deadlock it.
func (store *Store) notify() {
	defer close(store.hinted)
	for sequence := range store.hints {
		store.committed(sequence)
	}
}

// hint hands the newest created sequence to notify, replacing one it has not
// taken yet; only the committer sends, so it never blocks.
func (store *Store) hint(sequence int64) {
	if store.hints == nil {
		return
	}
	select {
	case <-store.hints:
	default:
	}
	store.hints <- sequence
}

// stopHints ends notify once the committer has stopped, after the last hint.
func (store *Store) stopHints() {
	if store.hints == nil {
		return
	}
	store.hintsOnce.Do(func() { close(store.hints) })
	<-store.hinted
}

// loadOrCreate loads the world, first creating it in an empty bucket unless
// the store is read-only; an existing world costs no extra marker read.
func (store *Store) loadOrCreate(ctx context.Context) error {
	err := store.load(ctx)
	// Only a missing marker is a clear not-found; a missing referenced object
	// is an integrity failure and never a reason to create.
	if store.readOnly || !missing(err) {
		return err
	}
	created, err := ensureWorld(ctx, store.objects, store.worldID)
	if err != nil {
		return err
	}
	if created {
		// Loud on purpose: an empty bucket is normally a first install,
		// but it is also what a wrong bucket URL looks like.
		store.logger.Warn("created a new world in an empty bucket", "worldID", store.worldID)
	}
	return store.load(ctx)
}

// load is a cold start: the newest checkpoint that loads, each tried within
// one request's time, and every slot after it; no body is read. Replayed
// slots count toward the next checkpoint.
func (store *Store) load(ctx context.Context) error {
	started := store.now()
	base, err := store.loader(store.requestTimeout).loadBase(ctx)
	if err != nil {
		return err
	}
	return store.start(ctx, base, started)
}

func (store *Store) loader(timeout time.Duration) checkpointLoader {
	return checkpointLoader{objects: store.objects, worldID: store.worldID, workers: store.shardWorkers, logger: store.logger, timeout: timeout}
}

// start serves a checkpoint's snapshot with every slot after it applied and
// keeps the loaded one as an eager compactor's base. The replay reads the
// bucket outside installMu; a reload a commit overtook replays on first.
func (store *Store) start(ctx context.Context, base loadedBase, started time.Time) error {
	loaded := base.snap
	tip := store.derive(loaded)
	var applied []appliedSlot
	onSlot := func(slot *slotObject) { applied = append(applied, appliedOf(slot)) }
	options := replayOptions{worldID: store.worldID, workers: store.shardWorkers, onSlot: onSlot, reach: base.reach}
	for {
		if err := replay(ctx, store.objects, tip, options); err != nil {
			return err
		}
		store.installMu.Lock()
		if current := store.served.Load(); current == nil || current.snap.Sequence <= tip.Sequence {
			break
		}
		store.installMu.Unlock()
	}
	defer store.installMu.Unlock()
	store.skipTo(loaded.Sequence)
	for _, slot := range applied {
		store.report(slot)
	}
	// The snapshot rests on its own checkpoint, and an active section index
	// catches up to it; both are no-ops at open.
	store.adoption.Store(nil)
	if store.keepsBase() {
		store.compaction.base.Store(loaded)
	}
	store.served.Store(&served{snap: tip, confirmed: started})
	store.sections.reloaded()
	store.noteSlots(len(applied))
	return nil
}

// goBackground runs fn with the store's life unless Close has begun, in
// which case it reports false. Close waits for fn.
func (store *Store) goBackground(fn func(life context.Context)) bool {
	store.lifeMu.Lock()
	defer store.lifeMu.Unlock()
	if store.life.Err() != nil {
		return false
	}
	store.background.Go(func() { fn(store.life) })
	return true
}

// Close stops following, the compactor, the section index and background
// work, ends every watch, waits for a slot being created and refuses later
// writes with backend.ErrClosed; reads still work. Idempotent.
func (store *Store) Close() error {
	store.commits.close()
	store.lifeMu.Lock()
	store.end()
	store.lifeMu.Unlock()
	store.background.Wait()
	store.stopHints()
	store.stopCompaction()
	store.sections.close()
	if store.changes == nil {
		return nil
	}
	store.follower.stop()
	<-store.follower.done
	store.changes.Close()
	store.changeLog.close()
	return nil
}

func nilStore(objects blob.Store) bool {
	if objects == nil {
		return true
	}
	value := reflect.ValueOf(objects)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
