package bucketstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// OpenReadView starts a view; its first read pins one snapshot, the log's
// tip for a read by path or one confirmed within SearchFreshness for a
// catalog read. Reads on the view share the request timeout started here.
func (store *Store) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open read view: %w", err)
	}
	return &snapshotView{store: store, objects: store.objects, deadline: time.Now().Add(store.requestTimeout)}, nil
}

// boundedBy derives a context only when ctx would outlive the deadline; a
// request context usually carries an earlier one already.
func boundedBy(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if current, ok := ctx.Deadline(); ok && !current.After(deadline) {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// snapshotView is the contract view: one snapshot pinned at its first read,
// a context per call.
type snapshotView struct {
	store    *Store // nil when the snapshot is lent pinned
	objects  objectGetter
	deadline time.Time
	closed   atomic.Bool

	mu       sync.Mutex
	snapshot *snapshot
	// deps records a commit attempt's reads; nil for every other view.
	deps *dependencies
}

var _ backend.ReadView = (*snapshotView)(nil)

// Close ends the view; the immutable snapshot itself needs no release.
func (view *snapshotView) Close() error {
	view.closed.Store(true)
	return nil
}

// pin returns the view's snapshot, choosing it on the first read.
func (view *snapshotView) pin(ctx context.Context, class readClass) (*snapshot, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.snapshot == nil {
		pinned, err := view.store.current(ctx, class)
		if err != nil {
			return nil, err
		}
		view.snapshot = pinned
	}
	return view.snapshot, nil
}

// viewRead runs one read against the pinned snapshot under the call's context.
// A closed view answers backend.ErrViewClosed.
func viewRead[T any](ctx context.Context, view *snapshotView, class readClass, fn func(context.Context, *readView) (T, error)) (T, error) {
	var zero T
	if view.closed.Load() {
		return zero, backend.ErrViewClosed
	}
	callCtx, cancel := boundedBy(ctx, view.deadline)
	defer cancel()
	if err := callCtx.Err(); err != nil {
		return zero, err
	}
	pinned, err := view.pin(callCtx, class)
	if err != nil {
		return zero, fmt.Errorf("pin snapshot: %w", normalizeReadIntegrity(err))
	}
	return fn(callCtx, &readView{objects: view.objects, snapshot: pinned, deps: view.deps})
}

// ReadCurrent is one current document off a fresh view; views pin a
// snapshot, so no lock is held for it.
func (store *Store) ReadCurrent(ctx context.Context, reqPath string) (document *storefmt.Document, err error) {
	view, err := store.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	return view.Get(ctx, reqPath, 0)
}

func (view *snapshotView) Get(ctx context.Context, reqPath string, version int) (*storefmt.Document, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (*storefmt.Document, error) {
		return r.Get(ctx, reqPath, version)
	})
}

func (view *snapshotView) ListEntries(ctx context.Context, reqPath string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return viewRead(ctx, view, catalogRead, func(ctx context.Context, r *readView) ([]storefmt.DirEntry, error) {
		return r.listPage(ctx, reqPath, opts)
	})
}

// IsDir is exact: FETCH asks it before reading a version path.
func (view *snapshotView) IsDir(ctx context.Context, reqPath string) (bool, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (bool, error) { return r.IsDir(ctx, reqPath) })
}

func (view *snapshotView) Versions(ctx context.Context, reqPath string) ([]storefmt.VersionInfo, error) {
	return viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) ([]storefmt.VersionInfo, error) {
		return r.Versions(ctx, reqPath)
	})
}

func (view *snapshotView) VerifyChain(ctx context.Context, reqPath string) error {
	_, err := viewRead(ctx, view, exactRead, func(ctx context.Context, r *readView) (struct{}, error) {
		return struct{}{}, r.VerifyChain(ctx, reqPath)
	})
	return err
}

// Lookup in body mode answers from the section index version built for the
// pinned snapshot, waiting briefly for it; without one it refuses with
// backend.ErrBodyMatchUnavailable and the caller answers from the catalog.
func (view *snapshotView) Lookup(ctx context.Context, query string, options catalog.Options) ([]catalog.Result, error) {
	return viewRead(ctx, view, catalogRead, func(ctx context.Context, r *readView) ([]catalog.Result, error) {
		if !protocol.MatchSectioned(string(options.Match)) {
			return search(ctx, r.scan(), query, options)
		}
		version, err := view.sectionIndex(ctx, r, options.Stale)
		if err != nil {
			return nil, err
		}
		return search(ctx, version, query, options)
	})
}

// sectionIndex is the section index version covering the pinned snapshot,
// waited for briefly, or with stale the newest one built; a lent snapshot
// or none in time refuses with backend.ErrBodyMatchUnavailable.
func (view *snapshotView) sectionIndex(ctx context.Context, r *readView, stale bool) (*sectionVersion, error) {
	if view.store == nil {
		return nil, backend.ErrBodyMatchUnavailable
	}
	var version *sectionVersion
	if stale {
		version = view.store.sections.latest(ctx)
	} else {
		version, _ = view.store.sections.await(ctx, r.snapshot.Sequence)
	}
	if version == nil {
		return nil, backend.ErrBodyMatchUnavailable
	}
	return version, nil
}

// attemptView lends a commit attempt's snapshot to a precondition as an
// ordinary contract view, bounded by the attempt's own deadline; its reads
// join the attempt's dependencies.
func attemptView(ctx context.Context, view *readView) *snapshotView {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultRequestTimeout)
	}
	return &snapshotView{objects: view.objects, deadline: deadline, snapshot: view.snapshot, deps: view.deps}
}
