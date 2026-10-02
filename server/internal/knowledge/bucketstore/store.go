package bucketstore

import (
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
	// Logger receives section-index warnings; nil uses slog.Default.
	Logger *slog.Logger
	// ChangeRing enables WATCH: the hub keeps this many events from the
	// applied slots, under the log's sequence, and the store follows peers
	// until Close. Zero leaves WATCH off.
	ChangeRing int
	// Committed runs once for each slot this replica creates, with the slot's
	// last sequence, outside every lock; nil for none.
	Committed func(sequence int64)

	// followInterval overrides the backstop poll's period in tests.
	followInterval time.Duration
}

// Store serves one world from the log: the snapshot it last applied, and
// slots it creates to commit.
type Store struct {
	objects        blob.Store
	worldID        string
	id             string // names this store in the slots it writes
	requestTimeout time.Duration
	shardWorkers   int
	logger         *slog.Logger
	maxDocuments   int
	readOnly       bool
	served         atomic.Pointer[served]
	refreshMu      sync.Mutex
	// deriveMu serializes snapshot clones: btree.Clone writes to its source.
	deriveMu       sync.Mutex
	commits        commitQueue
	now            func() time.Time
	newOperationID func() (string, error)
	// peerSeen is when this store last applied another store's slot, in Unix
	// nanoseconds; jitter is the wait after a win while it is recent, and no
	// create starts before yieldUntil.
	peerSeen   atomic.Int64
	jitter     func() time.Duration
	yieldUntil atomic.Int64
	// pending is the slot being created and the snapshot it makes.
	pending atomic.Pointer[pendingSlot]

	changes   *changefeed.Hub
	committed func(sequence int64)
	follower  *follower // nil without WATCH
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
	if !validWorldID(options.WorldID) {
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
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.ShardWorkers == 0 {
		options.ShardWorkers = defaultShardWorkers
	}
	if options.followInterval == 0 {
		options.followInterval = followInterval
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	id, err := randomOperationID()
	if err != nil {
		return nil, fmt.Errorf("open bucket store: store ID: %w", err)
	}

	store := &Store{
		objects:        objects,
		worldID:        options.WorldID,
		id:             id,
		requestTimeout: options.RequestTimeout,
		shardWorkers:   options.ShardWorkers,
		logger:         options.Logger,
		maxDocuments:   options.MaxDocuments,
		readOnly:       options.ReadOnly,
		commits:        commitQueue{wake: make(chan struct{}, 1)},
		now:            time.Now,
		newOperationID: randomOperationID,
		jitter:         yieldDelay,
		committed:      options.Committed,
	}
	store.changes = newHub(store, options.ChangeRing)
	store.refreshMu.Lock()
	err = store.loadOrCreate(ctx, &options)
	store.refreshMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	if store.changes != nil {
		store.startFollowing(options.followInterval)
	}
	return store, nil
}

// loadOrCreate loads the world, first creating it in an empty bucket unless
// the store is read-only; an existing world costs no extra marker read.
func (store *Store) loadOrCreate(ctx context.Context, options *Options) error {
	err := store.load(ctx)
	// Only a missing marker is a clear not-found; a missing referenced object
	// is an integrity failure and never a reason to create.
	if options.ReadOnly || !errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrIntegrity) {
		return err
	}
	created, err := ensureWorld(ctx, store.objects, options.WorldID)
	if err != nil {
		return err
	}
	if created {
		// Loud on purpose: an empty bucket is normally a first install,
		// but it is also what a wrong bucket URL looks like.
		options.Logger.Warn("created a new world in an empty bucket", "worldID", options.WorldID)
	}
	return store.load(ctx)
}

// load is a cold start: the newest checkpoint within one request's time,
// every slot after it, then the section index, reading every live body once
// (ADR 0012).
func (store *Store) load(ctx context.Context) error {
	started := store.now()
	baseCtx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	loaded, err := loadBase(baseCtx, store.objects, store.worldID, store.shardWorkers)
	cancel()
	if err != nil {
		return err
	}
	// The hub starts at the checkpoint; replayed slots fill its ring.
	store.skipTo(loaded.Sequence)
	if err := replay(ctx, store.objects, loaded, replayOptions{worldID: store.worldID, workers: store.shardWorkers, onSlot: store.report}); err != nil {
		return err
	}
	reindex := make(map[string]struct{})
	loaded.Paths.Ascend(func(state *pathState) bool {
		if !state.Archived {
			reindex[state.Path] = struct{}{}
		}
		return true
	})
	if err := store.indexSections(ctx, loaded, reindex); err != nil {
		return err
	}
	store.served.Store(&served{snap: loaded, confirmed: started})
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

// refresh catches up to the log's tip. A caller that waited while another
// refresh probed after it asked takes that tip instead of probing again.
func (store *Store) refresh(ctx context.Context) (*snapshot, error) {
	asked := store.now()
	store.refreshMu.Lock()
	defer store.refreshMu.Unlock()
	if served := store.served.Load(); served.confirmed.After(asked) {
		return served.snap, nil
	}
	return store.catchUpLocked(ctx, nil)
}

// catchUpFrom catches up through a slot this store lost the race for; it
// holds the winner already, so that slot is not read again.
func (store *Store) catchUpFrom(ctx context.Context, winner slotRead) (*snapshot, error) {
	store.refreshMu.Lock()
	defer store.refreshMu.Unlock()
	return store.catchUpLocked(ctx, &winner)
}

// catchUpLocked probes the slot after the served snapshot, unless known is
// that slot; when there is one it applies it and every slot a listing names
// after it, read in parallel.
func (store *Store) catchUpLocked(ctx context.Context, known *slotRead) (*snapshot, error) {
	base := store.served.Load().snap
	probed := store.now()
	var first slotRead
	if known != nil && known.slot.First == base.Sequence+1 {
		first = *known
	} else {
		read, err := readSlot(ctx, store.objects, store.worldID, base.Sequence+1)
		if errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrIntegrity) {
			store.served.Store(&served{snap: base, confirmed: probed})
			return base, nil
		}
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		first = read
	}
	var next *snapshot
	reindex := make(map[string]struct{})
	if pending := store.pending.Load(); pending != nil && pending.hash == first.hash && pending.base.Sequence == base.Sequence {
		next = store.derive(pending.next)
	} else {
		next = store.derive(base)
		if err := next.applySlot(first, reindex); err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
	}
	applied := []*slotObject{first.slot}
	// A strongly consistent listing names every slot created before it began.
	listed := store.now()
	err := replay(ctx, store.objects, next, replayOptions{
		worldID: store.worldID, workers: store.shardWorkers, reindex: reindex,
		onSlot: func(slot *slotObject) { applied = append(applied, slot) },
	})
	var broken *applyError
	if errors.As(err, &broken) {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// What applied is kept when a read fails, so a replica far behind gains
	// ground on every attempt; the index runs past the caller's deadline.
	indexCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), store.requestTimeout)
	defer cancel()
	if indexErr := store.indexSections(indexCtx, next, reindex); indexErr != nil {
		return nil, errors.Join(err, indexErr)
	}
	if err != nil {
		// Not the tip, but no slot existed past base when it was confirmed.
		store.install(next, applied, store.served.Load().confirmed)
		return nil, fmt.Errorf("refresh: %w", err)
	}
	store.install(next, applied, listed)
	return next, nil
}

// install serves next, confirmed at the given time, and publishes the events
// of the slots that built it; refreshMu orders events in the log's order.
func (store *Store) install(next *snapshot, applied []*slotObject, confirmed time.Time) {
	store.served.Store(&served{snap: next, confirmed: confirmed})
	for _, slot := range applied {
		if slot.Store != store.id {
			store.peerSeen.Store(store.now().UnixNano())
		}
		store.report(slot)
	}
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
