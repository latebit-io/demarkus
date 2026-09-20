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
	"errors"
	"fmt"
	"log"
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

// warnf reports a tokens file that could not be used; tests replace it.
var warnf = log.Printf

// LoadDefault loads tokens from the default path (~/.mark/tokens.toml).
// A broken file is reported and reads as empty, so requests go out unauthenticated.
func LoadDefault() *Store {
	path := DefaultPath()
	s, err := Load(path)
	if err != nil {
		warnf("tokens: no stored tokens in use: %v", err)
		return &Store{path: path, tokens: make(map[string]entry)}
	}
	return s
}

// Credential is a flag or DEMARKUS_AUTH token bound to the one host it was issued for.
type Credential struct {
	Explicit string // flag or caller value; empty falls back to DEMARKUS_AUTH
	Origin   string // host:port the token belongs to; empty means no host
}

// Resolve returns the token for host: the credential on its origin, else the
// stored token. Foreign hosts reached through links never see the credential.
// The store can be nil (skips stored token lookup).
func Resolve(cred Credential, host string, store *Store) string {
	if cred.Origin != "" && host == cred.Origin {
		if cred.Explicit != "" {
			return cred.Explicit
		}
		if env := os.Getenv("DEMARKUS_AUTH"); env != "" {
			return env
		}
	}
	return store.Get(host)
}

// Get returns the raw token for the given host:port, or empty string if not found.
// Safe to call on a nil Store.
func (s *Store) Get(host string) string {
	if s == nil {
		return ""
	}
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

// save replaces the file by rename so a concurrent reader never sees it truncated.
func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create tokens directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tokens-*.tmp")
	if err != nil {
		return fmt.Errorf("create tokens temp file: %w", err)
	}
	if err := writeTokens(tmp, s.tokens); err != nil {
		return errors.Join(err, removeTemp(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return errors.Join(fmt.Errorf("replace tokens file: %w", err), removeTemp(tmp.Name()))
	}
	return nil
}

// writeTokens encodes, syncs and closes f. CreateTemp already made it 0600.
func writeTokens(f *os.File, tokens map[string]entry) error {
	if err := toml.NewEncoder(f).Encode(tokens); err != nil {
		return errors.Join(fmt.Errorf("write tokens file: %w", err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync tokens file: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tokens file: %w", err)
	}
	return nil
}

func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove temp file %s: %w", name, err)
	}
	return nil
}
