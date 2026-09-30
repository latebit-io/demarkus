// Package filestore gives the protocol file store atomic handler snapshots.
package filestore

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/latebit-io/demarkus/protocol"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// ErrClosed means a write reached the store after Close.
var ErrClosed = errors.New("filestore: store is closed")

// Store keeps file data, hash state, and catalog state behind one lock.
type Store struct {
	mu        sync.RWMutex
	documents *protocolstore.Store
	catalog   *catalog.Catalog
	closed    bool

	// WATCH state, fixed at Open: the hub and the journal that makes its
	// sequence durable. The hub's head is the last sequence assigned.
	changes *changefeed.Hub
	journal *journal
}

var _ backend.Store = (*Store)(nil)

// Options configures WATCH on a file store.
type Options struct {
	// ChangeRing enables WATCH with a hub of this many events; zero leaves
	// WATCH off.
	ChangeRing int
	// Logger receives change journal notices; nil uses slog.Default.
	Logger *slog.Logger
}

// New wraps one file store and its derived catalog, without WATCH.
func New(documents *protocolstore.Store, lookup *catalog.Catalog) *Store {
	return &Store{documents: documents, catalog: lookup}
}

// Open is New plus WATCH (backend.ChangeSource): the change journal beside
// the documents is restored against the hash index's fingerprint, so build
// the index first or every open starts a new epoch.
func Open(documents *protocolstore.Store, lookup *catalog.Catalog, opts Options) (*Store, error) {
	s := New(documents, lookup)
	if opts.ChangeRing <= 0 {
		return s, nil
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	journal, err := openJournal(documents.Root(), documents.Fingerprint(), opts.ChangeRing, logger)
	if err != nil {
		return nil, err
	}
	hub := changefeed.New(journal.epoch, opts.ChangeRing)
	for _, ev := range journal.tail {
		hub.PublishAt(ev)
	}
	restored := len(journal.tail)
	journal.tail, journal.retained = nil, hub.Retained
	s.changes, s.journal = hub, journal
	logger.Info("change journal restored", "epoch", hub.Epoch(), "events", restored, "head", hub.Head().Seq)
	return s, nil
}

// Changes is the hub Open created, or nil when WATCH is off.
func (s *Store) Changes() *changefeed.Hub { return s.changes }

// Close ends every watch, releases the change journal and refuses later
// writes; the documents need no close.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.changes == nil {
		return nil
	}
	s.changes.Close()
	return s.journal.close()
}

// hint numbers and publishes one commit under the caller's write lock,
// journal first.
func (s *Store) hint(path string, doc *storefmt.Document, op string) {
	if s.changes == nil {
		return
	}
	ev := changefeed.DocumentEvent(path, doc, op)
	ev.Seq = s.changes.Head().Seq + 1
	s.journal.append(ev, s.documents.Fingerprint())
	s.changes.PublishAt(ev)
}

// call runs one store operation unless ctx is already done, and reports a
// missing file as the contract's not found. Local disk reads cannot be
// interrupted, so a context only stops a call before it starts.
func call[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	value, err := fn()
	return value, backend.FromNotExist(err)
}

// writeSpec carries the request into the file store. Its precondition runs
// under the write lock, reading the same state the write commits against.
func (s *Store) writeSpec(ctx context.Context, req *backend.WriteRequest) *storefmt.WriteSpec {
	spec := &storefmt.WriteSpec{Path: req.Path, ExpectedVersion: req.ExpectedVersion, Content: req.Content, Metadata: req.Metadata}
	if req.Precondition != nil {
		spec.Check = func(write storefmt.PreparedWrite) error {
			return req.Precondition(ctx, reader{store: s}, write)
		}
	}
	return spec
}

// Publish commits document and catalog state under one lock.
func (s *Store) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	return s.commit(ctx, &req, protocol.OpPublish, s.documents.WriteChecked)
}

// Append commits document and catalog state under one lock.
func (s *Store) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	return s.commit(ctx, &req, protocol.OpAppend, s.documents.AppendChecked)
}

// commit runs one write under the lock; a version that landed, durable or
// not, reaches the catalog and the watchers before its error is returned.
func (s *Store) commit(ctx context.Context, req *backend.WriteRequest, op string, write func(*storefmt.WriteSpec) (*storefmt.Document, error)) (*storefmt.Document, error) {
	return call(ctx, func() (*storefmt.Document, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return nil, ErrClosed
		}
		document, err := write(s.writeSpec(ctx, req))
		if storefmt.Committed(err) {
			s.catalog.Put(req.Path, document.Metadata, document.Content, document.Modified)
			s.hint(req.Path, document, op)
		}
		return document, err
	})
}

// SetArchived commits archive and catalog state under one lock.
func (s *Store) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	return call(ctx, func() (backend.ArchiveResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return backend.ArchiveResult{}, ErrClosed
		}
		spec := &storefmt.ArchiveSpec{ArchiveChange: storefmt.ArchiveChange{Path: req.Path, Archived: req.Archived}}
		if req.Precondition != nil {
			// Under the write lock, reading the state the change commits against.
			spec.Check = func(change storefmt.ArchiveChange) error {
				return req.Precondition(ctx, reader{store: s}, change)
			}
		}
		document, changed, err := s.documents.ArchiveChecked(spec)
		switch {
		case !changed:
		case req.Archived:
			s.catalog.Remove(req.Path)
		default:
			s.catalog.Put(req.Path, document.Metadata, document.Content, document.Modified)
		}
		if changed {
			s.hint(req.Path, document, changefeed.ArchiveOp(req.Archived))
		}
		return backend.ArchiveResult{Document: document, Changed: changed}, err
	})
}

// OpenReadView holds a read lock until the request closes the view.
func (s *Store) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	return &readView{reader: reader{store: s}}, nil
}

// reader reads the store under a lock its caller holds. It has no Close, so a
// precondition lent one under the write lock cannot release anything.
type reader struct{ store *Store }

func (r reader) Get(ctx context.Context, path string, version int) (*storefmt.Document, error) {
	return call(ctx, func() (*storefmt.Document, error) { return r.store.documents.Get(path, version) })
}

func (r reader) ListEntries(ctx context.Context, path string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return call(ctx, func() ([]storefmt.DirEntry, error) { return r.store.documents.ListEntries(path, opts) })
}

func (r reader) IsDir(ctx context.Context, path string) (bool, error) {
	return call(ctx, func() (bool, error) { return r.store.documents.IsDir(path) })
}

func (r reader) Versions(ctx context.Context, path string) ([]storefmt.VersionInfo, error) {
	return call(ctx, func() ([]storefmt.VersionInfo, error) { return r.store.documents.Versions(path) })
}

func (r reader) LookupHash(ctx context.Context, hash string) (string, error) {
	return call(ctx, func() (string, error) { return r.store.documents.LookupHashResult(hash) })
}

func (r reader) VerifyChain(ctx context.Context, path string) error {
	_, err := call(ctx, func() (struct{}, error) { return struct{}{}, r.store.documents.VerifyChain(path) })
	return err
}

func (r reader) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	return call(ctx, func() ([]catalog.Result, error) { return r.store.catalog.Lookup(query, opts) })
}

// readView owns the store's read lock until Close. gate lets Close wait for
// reads already admitted, so none runs after the lock is gone.
type readView struct {
	reader reader
	gate   sync.RWMutex
	closed bool
}

// admit runs one read while the view is open.
func admit[T any](view *readView, fn func(reader) (T, error)) (T, error) {
	view.gate.RLock()
	defer view.gate.RUnlock()
	if view.closed {
		var zero T
		return zero, backend.ErrViewClosed
	}
	return fn(view.reader)
}

func (view *readView) Get(ctx context.Context, path string, version int) (*storefmt.Document, error) {
	return admit(view, func(r reader) (*storefmt.Document, error) { return r.Get(ctx, path, version) })
}

func (view *readView) ListEntries(ctx context.Context, path string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return admit(view, func(r reader) ([]storefmt.DirEntry, error) { return r.ListEntries(ctx, path, opts) })
}

func (view *readView) IsDir(ctx context.Context, path string) (bool, error) {
	return admit(view, func(r reader) (bool, error) { return r.IsDir(ctx, path) })
}

func (view *readView) Versions(ctx context.Context, path string) ([]storefmt.VersionInfo, error) {
	return admit(view, func(r reader) ([]storefmt.VersionInfo, error) { return r.Versions(ctx, path) })
}

func (view *readView) LookupHash(ctx context.Context, hash string) (string, error) {
	return admit(view, func(r reader) (string, error) { return r.LookupHash(ctx, hash) })
}

func (view *readView) VerifyChain(ctx context.Context, path string) error {
	_, err := admit(view, func(r reader) (struct{}, error) { return struct{}{}, r.VerifyChain(ctx, path) })
	return err
}

func (view *readView) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	return admit(view, func(r reader) ([]catalog.Result, error) { return r.Lookup(ctx, query, opts) })
}

// Close releases the read lock once, after the last admitted read returns.
func (view *readView) Close() error {
	view.gate.Lock()
	defer view.gate.Unlock()
	if !view.closed {
		view.closed = true
		view.reader.store.mu.RUnlock()
	}
	return nil
}
