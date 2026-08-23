package auth

import (
	"errors"
	"sync/atomic"
)

// Source atomically publishes token stores loaded from one file.
type Source struct {
	path    string
	current atomic.Pointer[TokenStore]
}

// OpenSource opens path and publishes its initial token store.
// An empty path creates a source with no token store.
func OpenSource(path string) (*Source, error) {
	source := &Source{path: path}
	if path == "" {
		return source, nil
	}
	if err := source.Reload(); err != nil {
		return nil, err
	}
	return source, nil
}

// Path returns the configured token file path.
func (s *Source) Path() string {
	return s.path
}

// Current returns the last successfully loaded token store.
func (s *Source) Current() *TokenStore {
	return s.current.Load()
}

// Reload loads and publishes a complete replacement token store.
func (s *Source) Reload() error {
	store, err := s.Load()
	if err != nil {
		return err
	}
	if store != nil {
		s.current.Store(store)
	}
	return nil
}

// Load reads a complete replacement without publishing it.
func (s *Source) Load() (*TokenStore, error) {
	if s.path == "" {
		return nil, nil
	}
	store, err := LoadTokens(s.path)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// Publish atomically replaces the current token store with a staged load.
func (s *Source) Publish(store *TokenStore) error {
	if store == nil {
		return errors.New("token store is nil")
	}
	s.current.Store(store)
	return nil
}
