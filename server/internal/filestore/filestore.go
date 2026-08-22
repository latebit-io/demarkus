// Package filestore gives the protocol file store atomic handler snapshots.
package filestore

import (
	"sync"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// Store keeps file data, hash state, and catalog state behind one lock.
type Store struct {
	mu        sync.RWMutex
	documents *protocolstore.Store
	catalog   *catalog.Catalog
}

// New wraps one file store and its derived catalog.
func New(documents *protocolstore.Store, lookup *catalog.Catalog) *Store {
	return &Store{documents: documents, catalog: lookup}
}

// Get reads one document version under a short-lived snapshot lock.
func (s *Store) Get(path string, version int) (*protocolstore.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.Get(path, version)
}

// ListEntries reads one directory under a short-lived snapshot lock.
func (s *Store) ListEntries(path string, includeArchived bool) ([]protocolstore.DirEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.ListEntries(path, includeArchived)
}

// IsDir checks path topology under a short-lived snapshot lock.
func (s *Store) IsDir(path string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.IsDir(path)
}

// Versions reads retained history under a short-lived snapshot lock.
func (s *Store) Versions(path string) ([]protocolstore.VersionInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.Versions(path)
}

// CurrentVersion reads the current version under a short-lived snapshot lock.
func (s *Store) CurrentVersion(path string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.CurrentVersion(path)
}

// LookupHash resolves a body hash under a short-lived snapshot lock.
func (s *Store) LookupHash(hash string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.LookupHash(hash)
}

// VerifyChain checks retained history under a short-lived snapshot lock.
func (s *Store) VerifyChain(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents.VerifyChain(path)
}

// Lookup reads the catalog under a short-lived snapshot lock.
func (s *Store) Lookup(query string, opts catalog.Options) ([]catalog.Result, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catalog.Lookup(query, opts)
}

// WriteVersion commits document and catalog state under one lock.
func (s *Store) WriteVersion(path string, expected int, body []byte, meta map[string]string) (*protocolstore.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.documents.WriteVersion(path, expected, body, meta)
	if err == nil {
		s.catalog.Put(path, document.Metadata, document.Content, document.Modified)
	}
	return document, err
}

// Append commits document and catalog state under one lock.
func (s *Store) Append(path string, expected int, body []byte, meta map[string]string) (*protocolstore.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, err := s.documents.Append(path, expected, body, meta)
	if err == nil {
		s.catalog.Put(path, document.Metadata, document.Content, document.Modified)
	}
	return document, err
}

// Archive commits archive and catalog state under one lock.
func (s *Store) Archive(path string, archived bool) (*protocolstore.Document, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, changed, err := s.documents.Archive(path, archived)
	if err != nil || !changed {
		return document, changed, err
	}
	if archived {
		s.catalog.Remove(path)
		return document, true, nil
	}
	s.catalog.Put(path, document.Metadata, document.Content, document.Modified)
	return document, true, nil
}

// Put is a no-op because mutation methods update the catalog atomically.
func (s *Store) Put(string, map[string]string, []byte, time.Time) {}

// Remove is a no-op because Archive updates the catalog atomically.
func (s *Store) Remove(string) {}

// OpenReadView holds a read lock until the request closes the view.
func (s *Store) OpenReadView() (backend.ReadView, error) {
	s.mu.RLock()
	return &readView{store: s}, nil
}

type readView struct {
	store *Store
	once  sync.Once
}

func (view *readView) Get(path string, version int) (*protocolstore.Document, error) {
	return view.store.documents.Get(path, version)
}

func (view *readView) ListEntries(path string, includeArchived bool) ([]protocolstore.DirEntry, error) {
	return view.store.documents.ListEntries(path, includeArchived)
}

func (view *readView) IsDir(path string) (bool, error) {
	return view.store.documents.IsDir(path)
}

func (view *readView) Versions(path string) ([]protocolstore.VersionInfo, error) {
	return view.store.documents.Versions(path)
}

func (view *readView) LookupHash(hash string) (string, error) {
	return view.store.documents.LookupHash(hash)
}

func (view *readView) VerifyChain(path string) error {
	return view.store.documents.VerifyChain(path)
}

func (view *readView) Lookup(query string, opts catalog.Options) ([]catalog.Result, error) {
	return view.store.catalog.Lookup(query, opts)
}

func (view *readView) Close() error {
	view.once.Do(view.store.mu.RUnlock)
	return nil
}
