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
			return req.Precondition(ctx, &readView{store: s}, write)
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
func (s *Store) SetArchived(ctx context.Context, path string, archived bool) (backend.ArchiveResult, error) {
	return call(ctx, func() (backend.ArchiveResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		document, changed, err := s.documents.ArchiveResult(path, archived)
		switch {
		case !changed:
		case archived:
			s.catalog.Remove(path)
		default:
			s.catalog.Put(path, document.Metadata, document.Content, document.Modified)
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
	return &readView{store: s}, nil
}

type readView struct {
	store *Store
	once  sync.Once
}

func (view *readView) Get(ctx context.Context, path string, version int) (*storefmt.Document, error) {
	return call(ctx, func() (*storefmt.Document, error) { return view.store.documents.Get(path, version) })
}

func (view *readView) ListEntries(ctx context.Context, path string, opts storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	return call(ctx, func() ([]storefmt.DirEntry, error) { return view.store.documents.ListEntries(path, opts) })
}

func (view *readView) IsDir(ctx context.Context, path string) (bool, error) {
	return call(ctx, func() (bool, error) { return view.store.documents.IsDir(path) })
}

func (view *readView) Versions(ctx context.Context, path string) ([]storefmt.VersionInfo, error) {
	return call(ctx, func() ([]storefmt.VersionInfo, error) { return view.store.documents.Versions(path) })
}

func (view *readView) LookupHash(ctx context.Context, hash string) (string, error) {
	return call(ctx, func() (string, error) { return view.store.documents.LookupHashResult(hash) })
}

func (view *readView) VerifyChain(ctx context.Context, path string) error {
	_, err := call(ctx, func() (struct{}, error) { return struct{}{}, view.store.documents.VerifyChain(path) })
	return err
}

func (view *readView) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	return call(ctx, func() ([]catalog.Result, error) { return view.store.catalog.Lookup(query, opts) })
}

func (view *readView) Close() error {
	view.once.Do(view.store.mu.RUnlock)
	return nil
}
