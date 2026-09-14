package fedcrawl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// schemaVersion is the on-disk format version. Increment on breaking changes.
const schemaVersion = 1

// ServerState tracks crawl state for a single server.
type ServerState struct {
	Host          string    `json:"host"`           // host:port (e.g., "example.com:6309")
	DiscoveredAt  time.Time `json:"discovered_at"`  // When this server was first seen
	LastCrawled   time.Time `json:"last_crawled"`   // When this server was last crawled
	DocumentCount int       `json:"document_count"` // Documents discovered on this server
}

// URLState tracks visit state for a single URL.
type URLState struct {
	Observation graph.Observation `json:"observation,omitzero"`
	URL         string            `json:"url"`
	Etag        string            `json:"etag,omitempty"`         // For conditional fetch
	LastVisited time.Time         `json:"last_visited"`           // When this URL was last fetched
	Status      string            `json:"status"`                 // Last known status (ok, not-found, error)
	ContentHash string            `json:"content_hash,omitempty"` // SHA-256 content hash
}

// stateDocument is the on-disk JSON envelope.
type stateDocument struct {
	Version int                    `json:"version"`
	Servers map[string]ServerState `json:"servers"` // keyed by host
	URLs    map[string]URLState    `json:"urls"`    // keyed by full URL
}

// State is the persistent crawl state.
type State struct {
	path    string
	mu      sync.RWMutex
	servers map[string]ServerState
	urls    map[string]URLState
}

// DefaultStatePath returns the default state file location (~/.mark/fedcrawl.json).
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "fedcrawl.json")
}

// LoadState reads state from disk. Returns an empty state if the file
// does not exist. Returns an error for other I/O or parse failures.
func LoadState(path string) (*State, error) {
	if path == "" {
		return nil, errors.New("fedcrawl: empty state path")
	}

	s := &State{
		path:    path,
		servers: make(map[string]ServerState),
		urls:    make(map[string]URLState),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read state %q: %w", path, err)
	}

	var doc stateDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse state %q: %w", path, err)
	}
	if doc.Version != schemaVersion {
		return nil, fmt.Errorf("state %q: unsupported schema version %d (expected %d)", path, doc.Version, schemaVersion)
	}

	// Guard against nil maps from JSON (null/omitted keys)
	if doc.Servers != nil {
		s.servers = doc.Servers
	}
	if doc.URLs != nil {
		for key := range doc.URLs {
			value := doc.URLs[key]
			key = links.CanonicalURL(key)
			value.URL = key
			if previous, ok := s.urls[key]; !ok || previous.LastVisited.Before(value.LastVisited) {
				s.urls[key] = value
			}
		}
	}

	return s, nil
}

// Save writes the state to disk atomically (write tmp, rename).
func (s *State) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc := stateDocument{
		Version: schemaVersion,
		Servers: s.servers,
		URLs:    s.urls,
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// RecordVisit records a URL visit with etag and content hash.
func (s *State) RecordVisit(url, etag, status, contentHash string) {
	url = links.CanonicalURL(url)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.urls[url] = URLState{
		URL:         url,
		Etag:        etag,
		LastVisited: time.Now().UTC(),
		Status:      status,
		ContentHash: contentHash,
	}
}

// RecordObservation retains last-good source evidence when a response fails.
func (s *State) RecordObservation(url string, response *protocol.Response) {
	url = links.CanonicalURL(url)
	s.mu.Lock()
	defer s.mu.Unlock()
	observation := graph.Observe(url, response.Metadata)
	observation.Complete = graph.SourceComplete(&graph.Node{Status: response.Status})
	previous := s.urls[url]
	previous.URL, previous.LastVisited = url, observation.AttemptedAt
	previous.Observation.AttemptedAt = observation.AttemptedAt
	switch {
	case !observation.Complete:
		previous.Observation.Problem = "incomplete"
	case graph.CompareRevision(&observation, &previous.Observation) == "older":
		previous.Observation.Problem = "revision-regression"
	default:
		previous.Status = response.Status
		previous.Etag, previous.ContentHash = observation.Etag, response.Metadata["content-hash"]
		previous.Observation = observation
	}
	s.urls[url] = previous
}

// RecordServer records a server discovery or updates its crawl timestamp.
func (s *State) RecordServer(host string, documentCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	existing, ok := s.servers[host]
	if ok {
		existing.LastCrawled = now
		existing.DocumentCount = documentCount
		s.servers[host] = existing
	} else {
		s.servers[host] = ServerState{
			Host:          host,
			DiscoveredAt:  now,
			LastCrawled:   now,
			DocumentCount: documentCount,
		}
	}
}

// GetURL returns state for a URL, or nil if not visited.
func (s *State) GetURL(url string) *URLState {
	url = links.CanonicalURL(url)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u, ok := s.urls[url]; ok {
		return &u
	}
	return nil
}

// GetServer returns state for a server, or nil if not known.
func (s *State) GetServer(host string) *ServerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sv, ok := s.servers[host]; ok {
		return &sv
	}
	return nil
}

// KnownServers returns all known server hosts.
func (s *State) KnownServers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hosts := make([]string, 0, len(s.servers))
	for h := range s.servers {
		hosts = append(hosts, h)
	}
	return hosts
}

// ShouldFetch returns true if the URL should be fetched (not visited or
// etag changed). If visited, returns the previous etag for conditional fetch.
func (s *State) ShouldFetch(url string) (shouldFetch bool, previousEtag string) {
	url = links.CanonicalURL(url)
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.urls[url]
	if !ok {
		return true, ""
	}
	// Always fetch if no etag was recorded (e.g., previous error).
	if u.Etag == "" {
		return true, ""
	}
	// For now, always re-fetch. In the future, we can use if-none-match
	// to skip unchanged content. Return the etag so caller can decide.
	return true, u.Etag
}

// AllContentHashes returns all collected content hashes with their URLs.
func (s *State) AllContentHashes() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hashes := make(map[string]string, len(s.urls))
	for url := range s.urls {
		u := s.urls[url]
		if u.ContentHash != "" {
			hashes[u.ContentHash] = url
		}
	}
	return hashes
}

// ServerCount returns the number of known servers.
func (s *State) ServerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.servers)
}

// URLCount returns the number of visited URLs.
func (s *State) URLCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}
