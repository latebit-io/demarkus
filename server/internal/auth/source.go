package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
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
	// the load: a Kubernetes Secret may be projected after the world opens.
	// A file that vanishes revokes its tokens; their read paths stay protected.
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
	// protected is each file's read patterns as last loaded. A file gone
	// missing keeps protecting them: a path is public only when no token
	// covers it, so dropping them with the file would publish its paths.
	mu        sync.Mutex
	protected map[string][]string
}

// OpenSource opens the configured files and publishes the initial store.
func OpenSource(config SourceConfig) (*Source, error) {
	source := &Source{config: config, files: config.Files(), protected: map[string][]string{}}
	if len(source.files) == 0 {
		return source, nil
	}
	if err := source.Reload(); err != nil {
		return nil, err
	}
	if config.Logger != nil && config.Optional {
		for _, file := range source.files {
			if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
				config.Logger.Warn("auth: tokens file not found, starting without its tokens; paths its read tokens would protect are public until it appears", "path", file)
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
// set, its tokens are the union of the files present on disk.
func (s *Source) Load() (*TokenStore, error) {
	if len(s.files) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := make(map[string]Token)
	var held []string
	for _, file := range s.files {
		tokens, err := loadTokenFile(file)
		if err != nil {
			if s.config.Optional && errors.Is(err, os.ErrNotExist) {
				held = append(held, s.vanishedLocked(file)...)
				continue
			}
			return nil, err
		}
		s.protected[file] = collectReadPaths(tokens)
		for hash, token := range tokens {
			if existing, ok := merged[hash]; ok {
				return nil, fmt.Errorf("duplicate hash for labels %q and %q across tokens files", existing.Label, token.Label)
			}
			merged[hash] = token
		}
	}
	store := NewTokenStore(merged)
	store.readPaths = append(store.readPaths, held...)
	return store, nil
}

// vanishedLocked returns the read patterns a now missing file last held, so
// they stay protected with no token left to read them.
func (s *Source) vanishedLocked(file string) []string {
	held := s.protected[file]
	if len(held) > 0 && s.config.Logger != nil {
		s.config.Logger.Warn("auth: tokens file vanished; its tokens are revoked and the paths its read tokens protected stay protected until it returns or the server restarts", "path", file)
	}
	return held
}

// Publish atomically replaces the current token store with a staged load.
func (s *Source) Publish(store *TokenStore) error {
	if store == nil {
		return errors.New("token store is nil")
	}
	s.current.Store(store)
	return nil
}
