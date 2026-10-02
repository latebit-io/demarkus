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
	// Committed runs after each of this replica's own commits with its
	// sequence, outside every lock; nil for none.
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
	commitToken    chan struct{}
	now            func() time.Time
	newOperationID func() (string, error)

	changes   *changefeed.Hub
	committed func(sequence int64)
	follower  *follower // nil without WATCH
	closed    atomic.Bool
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
		commitToken:    make(chan struct{}, 1),
		now:            time.Now,
		newOperationID: randomOperationID,
		committed:      options.Committed,
	}
	store.changes = newHub(store, options.ChangeRing)
	store.commitToken <- struct{}{}
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
	if err := store.indexSections(ctx, loaded, reindex, nil); err != nil {
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

// catchUpLocked probes the slot after the served snapshot; when there is one
// it applies it and every slot a listing names after it, read in parallel.
// Bodies this replica just wrote are in fresh.
func (store *Store) catchUpLocked(ctx context.Context, fresh map[string][]byte) (*snapshot, error) {
	base := store.served.Load().snap
	probed := store.now()
	slot, hash, err := readSlot(ctx, store.objects, store.worldID, base.Sequence+1)
	if errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrIntegrity) {
		store.served.Store(&served{snap: base, confirmed: probed})
		return base, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	next := base.derive()
	reindex := make(map[string]struct{})
	if err := next.applySlot(slot, hash, reindex); err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	applied := []*slotObject{slot}
	// A strongly consistent listing names every slot created before it began.
	listed := store.now()
	err = replay(ctx, store.objects, next, replayOptions{
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
	if indexErr := store.indexSections(indexCtx, next, reindex, fresh); indexErr != nil {
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
		store.report(slot)
	}
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
