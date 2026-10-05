package bucketstore

import (
	"cmp"
	"context"
	"errors"
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

// Options configures one world store.
type Options struct {
	WorldID        string
	RequestTimeout time.Duration
	ShardWorkers   int
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
	logger          *slog.Logger
	maxDocuments    int
	readOnly        bool
	served          atomic.Pointer[served]
	// probing is the probe of the log's tip in flight, which refreshes share
	// (one bucket reader at a time); nil when none runs.
	probeMu sync.Mutex
	probing chan struct{}
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

// served is the snapshot this replica serves and when it was last confirmed
// as the log's tip: a probe that started then found no later slot.
type served struct {
	snap      *snapshot
	confirmed time.Time
}

// pendingSlot is a slot this store is creating: a refresh that reads it before
// the create returns installs next rather than applying and indexing it again.
type pendingSlot struct {
	base, next *snapshot
	hash       string
}

// readClass is how fresh a view's first read needs its snapshot.
type readClass int

const (
	// exactRead sees every slot created before the read (reads by path).
	exactRead readClass = iota
	// catalogRead may use a snapshot confirmed within SearchFreshness.
	catalogRead
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

// load is a cold start: the newest checkpoint within one request's time and
// every slot after it; no body is read. Replayed slots count toward the next
// checkpoint.
func (store *Store) load(ctx context.Context) error {
	started := store.now()
	baseCtx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	loaded, err := loadBase(baseCtx, store.objects, store.worldID, store.shardWorkers)
	cancel()
	if err != nil {
		return err
	}
	return store.start(ctx, loaded, started)
}

// start serves a checkpoint's snapshot with every slot after it applied and
// keeps the loaded one as the compactor's base. The replay reads the bucket
// outside installMu; a reload a commit overtook replays on before installing.
func (store *Store) start(ctx context.Context, loaded *snapshot, started time.Time) error {
	tip := store.derive(loaded)
	var applied []appliedSlot
	onSlot := func(slot *slotObject) { applied = append(applied, appliedOf(slot)) }
	options := replayOptions{worldID: store.worldID, workers: store.shardWorkers, onSlot: onSlot}
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
	if !store.readOnly {
		store.compaction.base.Store(loaded)
	}
	store.served.Store(&served{snap: tip, confirmed: started})
	store.sections.reloaded()
	store.noteSlots(len(applied))
	return nil
}

// current returns the snapshot a view's first read pins: for a catalog read
// one confirmed within SearchFreshness, otherwise the log's tip.
func (store *Store) current(ctx context.Context, class readClass) (*snapshot, error) {
	if served := store.served.Load(); class == catalogRead && store.now().Sub(served.confirmed) < backend.SearchFreshness {
		return served.snap, nil
	}
	return store.refresh(ctx)
}

// refresh catches up to the log's tip. Probes are a shared flight: a caller
// takes a tip confirmed after it asked, waits on the probe in flight no
// longer than its own context, and otherwise starts the next one.
func (store *Store) refresh(ctx context.Context) (*snapshot, error) {
	asked := store.now()
	if asked.Sub(store.served.Load().confirmed) > staleAfter || store.diverged.Load() {
		if err := store.reload(ctx); err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
	}
	for {
		if served := store.served.Load(); served.confirmed.After(asked) {
			return served.snap, nil
		}
		flight, started := store.joinProbe()
		if started {
			return store.probe(ctx, flight, asked)
		}
		select {
		case <-flight:
		case <-ctx.Done():
			return nil, fmt.Errorf("refresh: %w", ctx.Err())
		}
	}
}

// joinProbe returns the probe in flight, or a new one this caller runs.
func (store *Store) joinProbe() (flight chan struct{}, started bool) {
	store.probeMu.Lock()
	defer store.probeMu.Unlock()
	if store.probing == nil {
		store.probing = make(chan struct{})
		return store.probing, true
	}
	return store.probing, false
}

// probe runs the flight's catch-up and releases it on every exit, so a panic
// recovered above the store cannot leave later refreshes waiting on it.
func (store *Store) probe(ctx context.Context, flight chan struct{}, asked time.Time) (*snapshot, error) {
	defer func() {
		store.probeMu.Lock()
		store.probing = nil
		store.probeMu.Unlock()
		close(flight)
	}()
	return store.catchUp(ctx, nil, asked)
}

// catchUpFrom catches up through a slot this store lost the race for, outside
// the probe flight: it holds the winner already and reads only the listing
// after it, so a loser's recovery never waits behind a reader's probe.
func (store *Store) catchUpFrom(ctx context.Context, winner slotRead) (*snapshot, error) {
	return store.catchUp(ctx, &winner, time.Time{})
}

// catchUp brings the served snapshot to the log's tip, from its next slot or
// from known, one this store lost the race for. A commit installing meanwhile
// restarts it, unless that one was confirmed after asked.
func (store *Store) catchUp(ctx context.Context, known *slotRead, asked time.Time) (*snapshot, error) {
	for {
		current := store.served.Load()
		if !asked.IsZero() && current.confirmed.After(asked) {
			return current.snap, nil
		}
		base := current.snap
		if known != nil && known.slot.First != base.Sequence+1 {
			// The winner's slot is installed already; the rest is a probe.
			return store.refresh(ctx)
		}
		ahead, err := store.readAhead(ctx, base, known)
		if ahead.next == nil && err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		store.installMu.Lock()
		if store.served.Load().snap != base {
			store.installMu.Unlock()
			continue
		}
		switch {
		case ahead.next == nil:
			store.served.Store(&served{snap: base, confirmed: ahead.confirmed})
		case err != nil:
			// Not the tip, but what applied is kept, so a replica far behind
			// gains ground on every attempt.
			store.install(ahead.next, ahead.applied, current.confirmed)
		default:
			store.install(ahead.next, ahead.applied, ahead.confirmed)
		}
		store.installMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		return store.served.Load().snap, nil
	}
}

// ahead is what a catch-up read past base: next is nil when base is the tip,
// and confirmed is when the tip was observed.
type ahead struct {
	next      *snapshot
	applied   []appliedSlot
	confirmed time.Time
}

// readAhead reads the slots after base, from the bucket alone. A read that
// fails after some slots applied returns them with its error.
func (store *Store) readAhead(ctx context.Context, base *snapshot, known *slotRead) (ahead, error) {
	probed := store.now()
	var first slotRead
	if known != nil {
		first = *known
	} else {
		read, err := readSlot(ctx, store.objects, store.worldID, base.Sequence+1)
		if missing(err) {
			return ahead{confirmed: probed}, nil
		}
		if err != nil {
			return ahead{}, err
		}
		first = read
	}
	var next *snapshot
	if pending := store.pending.Load(); pending != nil && pending.hash == first.hash && pending.base.Sequence == base.Sequence {
		next = store.derive(pending.next)
	} else {
		next = store.derive(base)
		if err := next.applySlot(first); err != nil {
			return ahead{}, err
		}
	}
	applied := []appliedSlot{appliedOf(first.slot)}
	// A strongly consistent listing names every slot created before it began.
	listed := store.now()
	err := replay(ctx, store.objects, next, replayOptions{
		worldID: store.worldID, workers: store.shardWorkers,
		onSlot: func(slot *slotObject) { applied = append(applied, appliedOf(slot)) },
	})
	var broken *applyError
	if errors.As(err, &broken) {
		return ahead{}, err
	}
	return ahead{next: next, applied: applied, confirmed: listed}, err
}

// reloadFlight is one reload that stale refreshes wait on.
type reloadFlight struct {
	done chan struct{}
	err  error
}

// reload waits, up to ctx, for a shared load from the newest checkpoint that
// outlives the request, since at scale it takes most of one request's time.
// After Close, when nothing runs in the background, the caller loads it.
func (store *Store) reload(ctx context.Context) error {
	store.reloadMu.Lock()
	flight := store.reloading
	if flight == nil {
		flight = &reloadFlight{done: make(chan struct{})}
		if !store.goBackground(func(life context.Context) { store.runReload(life, flight) }) {
			store.reloadMu.Unlock()
			return store.reloadIfBehind(ctx)
		}
		store.reloading = flight
	}
	store.reloadMu.Unlock()
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (store *Store) runReload(life context.Context, flight *reloadFlight) {
	ctx, cancel := context.WithTimeout(life, checkpointTimeout)
	defer cancel()
	flight.err = store.reloadIfBehind(ctx)
	store.reloadMu.Lock()
	store.reloading = nil
	store.reloadMu.Unlock()
	close(flight.done)
}

// reloadIfBehind starts again from the newest checkpoint when one lies past
// the served snapshot: its next slot may have been collected, which a probe
// would take for the log's tip, and a commit would then fork the log.
func (store *Store) reloadIfBehind(ctx context.Context) error {
	base, diverged := store.served.Load().snap, store.diverged.Load()
	after := base.Sequence
	if diverged {
		after = 0 // the newest checkpoint, wherever it lies
	}
	newest, err := newestCheckpointSequence(ctx, store.objects, after)
	if err != nil || newest == base.Sequence && !diverged {
		return err
	}
	store.logger.Warn("reloading from the newest checkpoint", "world", store.worldID, "served", base.Sequence, "checkpoint", newest, "diverged", diverged)
	started := store.now()
	checkpoint, err := readCheckpoint(ctx, store.objects, store.worldID, newest)
	if err != nil {
		return err
	}
	loaded, err := loadCheckpoint(ctx, store.objects, checkpoint, store.shardWorkers)
	if err != nil {
		return err
	}
	if err := store.start(ctx, loaded, started); err != nil {
		return err
	}
	store.diverged.Store(false)
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

// install serves next, confirmed at the given time, and publishes the events
// of the slots that built it; installMu, which the caller holds, orders
// events and section steps in the log's order.
func (store *Store) install(next *snapshot, applied []appliedSlot, confirmed time.Time) {
	installed := store.rebased(next)
	store.served.Store(&served{snap: installed, confirmed: confirmed})
	store.releaseAdoption(installed)
	store.sections.installed(installed, applied)
	for _, slot := range applied {
		if slot.store != store.id {
			store.peerSlots.Add(1)
			store.commits.signal()
		}
		store.report(slot)
	}
	store.noteSlots(len(applied))
}

// derive starts a snapshot from s; see snapshot.derive.
func (store *Store) derive(s *snapshot) *snapshot {
	store.deriveMu.Lock()
	defer store.deriveMu.Unlock()
	return s.derive()
}

// poll refreshes the snapshot from the bucket, reporting what peers wrote.
func (store *Store) poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	_, err := store.refresh(ctx)
	return err
}

// servedSequence is the sequence of the snapshot this replica serves.
func (store *Store) servedSequence() int64 { return store.served.Load().snap.Sequence }

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
