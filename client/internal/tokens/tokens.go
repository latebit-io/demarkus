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
//
// A sibling ~/.mark/tokens.d/ directory holds one raw token per file named
// by host:port; mounted Secrets land there without a TOML render step.
package tokens

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type entry struct {
	Token string `toml:"token"`
}

// Store manages client-side auth tokens keyed by host:port.
type Store struct {
	path   string
	tokens map[string]entry
	// dir holds tokens.d entries; read only, never written back to tokens.toml.
	dir map[string]string
}

// DefaultPath returns the default tokens file path (~/.mark/tokens.toml).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "tokens.toml")
}

// Load reads a tokens file and its sibling tokens.d/ directory. Returns an
// empty store if neither exists yet. Returns an error if path is empty.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("tokens file path is empty (could not determine home directory)")
	}
	s := &Store{path: path, tokens: make(map[string]entry), dir: loadDir(DirPath(path))}
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

// DirPath returns the tokens.d directory beside a tokens file.
func DirPath(tokensFile string) string {
	return filepath.Join(filepath.Dir(tokensFile), "tokens.d")
}

// warnf reports a tokens file that could not be used; tests replace it.
var warnf = log.Printf

// LoadDefault loads tokens from ~/.mark/tokens.toml and ~/.mark/tokens.d/.
// A broken file is reported and reads as empty, so requests go out unauthenticated.
func LoadDefault() *Store {
	path := DefaultPath()
	s, err := Load(path)
	if err != nil {
		warnf("tokens: no stored tokens in use: %v", err)
		// A broken tokens.toml must not drop projected tokens.d entries.
		return &Store{path: path, tokens: make(map[string]entry), dir: loadDir(DirPath(path))}
	}
	return s
}

// loadDir reads one raw token per regular file in dir, keyed by file name.
// Dotfiles are skipped: kubelet keeps ..data and timestamped dirs beside
// the projected files. Unreadable entries are reported and skipped.
func loadDir(dir string) map[string]string {
	out := make(map[string]string)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			warnf("tokens: skipping tokens directory %q: %v", dir, err)
		}
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full) // follows the projected-volume symlink
		if err != nil {
			warnf("tokens: skipping %q: %v", full, err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			warnf("tokens: skipping %q: %v", full, err)
			continue
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			warnf("tokens: skipping %q: empty token", full)
			continue
		}
		out[name] = tok
	}
	return out
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

// Get returns the raw token for the given host:port, or empty string if not
// found. tokens.toml wins over tokens.d for the same host. Safe on a nil Store.
func (s *Store) Get(host string) string {
	if s == nil {
		return ""
	}
	if e, ok := s.tokens[host]; ok {
		return e.Token
	}
	return s.dir[host]
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

// Hosts returns a sorted list of all host:port entries across both sources.
func (s *Store) Hosts() []string {
	seen := make(map[string]struct{}, len(s.tokens)+len(s.dir))
	for h := range s.tokens {
		seen[h] = struct{}{}
	}
	for h := range s.dir {
		seen[h] = struct{}{}
	}
	hosts := make([]string, 0, len(seen))
	for h := range seen {
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

// Resolver answers Resolve for one credential and store; it is the client's
// fetch.TokenResolver. A nil Store skips the stored token lookup.
type Resolver struct {
	Credential Credential
	Store      *Store
}

// Token returns the token for host.
func (r Resolver) Token(host string) string { return Resolve(r.Credential, host, r.Store) }
