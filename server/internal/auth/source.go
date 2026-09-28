package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
)

// SourceConfig names the files one Source merges into a token store.
type SourceConfig struct {
	// TokensFile is the runtime tokens file, the one a broker appends to.
	// Empty with no StaticTokensFile means no auth: every store is nil.
	TokensFile string
	// StaticTokensFile holds operator-owned entries merged with TokensFile.
	// A hash present in both files is a load error.
	StaticTokensFile string
	// Optional lets a missing file contribute no tokens instead of failing
	// the load: on Kubernetes the Secret behind a file may be created after
	// the world opens, and the watcher reloads once it is projected.
	Optional bool
	// Logger reports files missing at open; nil discards.
	Logger *slog.Logger
}

// Files lists the configured files in load order, empties dropped.
func (config SourceConfig) Files() []string {
	var files []string
	for _, file := range []string{config.TokensFile, config.StaticTokensFile} {
		if file != "" {
			files = append(files, file)
		}
	}
	return files
}

// Source atomically publishes token stores merged from a world's files.
type Source struct {
	config  SourceConfig
	files   []string
	current atomic.Pointer[TokenStore]
}

// OpenSource opens the configured files and publishes the initial store.
func OpenSource(config SourceConfig) (*Source, error) {
	source := &Source{config: config, files: config.Files()}
	if len(source.files) == 0 {
		return source, nil
	}
	if err := source.Reload(); err != nil {
		return nil, err
	}
	if config.Logger != nil && config.Optional {
		for _, file := range source.files {
			if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
				config.Logger.Info("auth: tokens file not found, starting without its tokens", "path", file)
			}
		}
	}
	return source, nil
}

// Path returns the configured runtime token file path.
func (s *Source) Path() string {
	return s.config.TokensFile
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

// Load reads a complete replacement without publishing it. With Optional
// set, the store is exactly the union of the files present on disk.
func (s *Source) Load() (*TokenStore, error) {
	if len(s.files) == 0 {
		return nil, nil
	}
	merged := make(map[string]Token)
	for _, file := range s.files {
		tokens, err := loadTokenFile(file)
		if err != nil {
			if s.config.Optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for hash, token := range tokens {
			if existing, ok := merged[hash]; ok {
				return nil, fmt.Errorf("duplicate hash for labels %q and %q across tokens files", existing.Label, token.Label)
			}
			merged[hash] = token
		}
	}
	return NewTokenStore(merged), nil
}

// Publish atomically replaces the current token store with a staged load.
func (s *Source) Publish(store *TokenStore) error {
	if store == nil {
		return errors.New("token store is nil")
	}
	s.current.Store(store)
	return nil
}
