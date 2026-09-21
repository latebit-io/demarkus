// Package filestore gives the protocol file store atomic handler snapshots.
package filestore

import (
	"context"
	"sync"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Store keeps file data, hash state, and catalog state behind one lock.
type Store struct {
	mu        sync.RWMutex
	documents *protocolstore.Store
	catalog   *catalog.Catalog
}

var _ backend.Store = (*Store)(nil)

// New wraps one file store and its derived catalog.
func New(documents *protocolstore.Store, lookup *catalog.Catalog) *Store {
	return &Store{documents: documents, catalog: lookup}
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
	return call(ctx, func() (*storefmt.Document, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		document, err := s.documents.WriteChecked(s.writeSpec(ctx, &req))
		if err == nil {
			s.catalog.Put(req.Path, document.Metadata, document.Content, document.Modified)
		}
		return document, err
	})
}

// Append commits document and catalog state under one lock.
func (s *Store) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	return call(ctx, func() (*storefmt.Document, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		document, err := s.documents.AppendChecked(s.writeSpec(ctx, &req))
		if err == nil {
			s.catalog.Put(req.Path, document.Metadata, document.Content, document.Modified)
		}
		return document, err
	})
}

// SetArchived commits archive and catalog state under one lock.
func (s *Store) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	return call(ctx, func() (backend.ArchiveResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
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
