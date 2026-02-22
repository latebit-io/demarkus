// Package tokens provides client-side auth token storage per server host.
//
// Tokens are stored in a TOML file (default ~/.mark/tokens.toml) mapping
// host:port to raw auth tokens. This allows the client to auto-inject
// the correct token when publishing to a known server.
//
// TOML format:
//
//	["localhost:6309"]
//	token = "abc123..."
//
//	["demarkus.latebit.io:6309"]
//	token = "def456..."
package tokens

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

type entry struct {
	Token string `toml:"token"`
}

// Store manages client-side auth tokens keyed by host:port.
type Store struct {
	path   string
	tokens map[string]entry
}

// DefaultPath returns the default tokens file path (~/.mark/tokens.toml).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "tokens.toml")
}

// Load reads a tokens file from disk. Returns an empty store if the file
// does not exist yet. Returns an error if path is empty.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("tokens file path is empty (could not determine home directory)")
	}
	s := &Store{path: path, tokens: make(map[string]entry)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read tokens file %q: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if _, err := toml.Decode(string(data), &s.tokens); err != nil {
		return nil, fmt.Errorf("parse tokens file %q: %w", path, err)
	}
	return s, nil
}

// Get returns the raw token for the given host:port, or empty string if not found.
func (s *Store) Get(host string) string {
	e, ok := s.tokens[host]
	if !ok {
		return ""
	}
	return e.Token
}

// Set stores a token for the given host:port and writes to disk.
func (s *Store) Set(host, token string) error {
	s.tokens[host] = entry{Token: token}
	return s.save()
}

// Remove deletes the token for the given host:port and writes to disk.
func (s *Store) Remove(host string) error {
	delete(s.tokens, host)
	return s.save()
}

// Hosts returns a sorted list of all stored host:port entries.
func (s *Store) Hosts() []string {
	hosts := make([]string, 0, len(s.tokens))
	for h := range s.tokens {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create tokens directory: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open tokens file: %w", err)
	}
	if err := toml.NewEncoder(f).Encode(s.tokens); err != nil {
		_ = f.Close()
		return fmt.Errorf("write tokens file: %w", err)
	}
	return f.Close()
}
