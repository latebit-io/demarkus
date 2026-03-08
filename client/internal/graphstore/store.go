// Package graphstore provides persistent storage for the document link graph.
//
// The graph is stored as a JSON file at ~/.mark/graph.json. It records nodes
// (documents) and edges (links between documents) discovered by the graph
// crawler, along with etags and timestamps for incremental updates.
package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit/demarkus/client/internal/graph"
)

// schemaVersion is the on-disk format version. Increment on breaking changes.
const schemaVersion = 1

// StoredNode is a graph node with persistence metadata.
type StoredNode struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	LinkCount int       `json:"link_count"`
	Etag      string    `json:"etag,omitempty"`
	CrawledAt time.Time `json:"crawled_at"`
}

// StoredEdge is a directed link between two document URLs.
type StoredEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// document is the on-disk JSON envelope.
type document struct {
	Version int          `json:"version"`
	Nodes   []StoredNode `json:"nodes"`
	Edges   []StoredEdge `json:"edges"`
}

// Store is the persistent graph state.
type Store struct {
	path    string
	mu      sync.RWMutex
	nodes   map[string]*StoredNode
	edges   []StoredEdge
	edgeSet map[StoredEdge]struct{}
}

// DefaultPath returns the default graph store location (~/.mark/graph.json).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "graph.json")
}

// Load reads a graph store from disk. Returns an empty store if the file
// does not exist. Returns an error for other I/O or parse failures.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("graphstore: empty path")
	}

	s := &Store{
		path:    path,
		nodes:   make(map[string]*StoredNode),
		edgeSet: make(map[StoredEdge]struct{}),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read graph store %q: %w", path, err)
	}

	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse graph store %q: %w", path, err)
	}
	if doc.Version != schemaVersion {
		return nil, fmt.Errorf("graph store %q: unsupported schema version %d (expected %d)", path, doc.Version, schemaVersion)
	}

	for i := range doc.Nodes {
		n := doc.Nodes[i]
		s.nodes[n.URL] = &n
	}
	for _, e := range doc.Edges {
		if _, exists := s.edgeSet[e]; !exists {
			s.edgeSet[e] = struct{}{}
			s.edges = append(s.edges, e)
		}
	}

	return s, nil
}

// Save writes the graph store to disk atomically (write tmp, rename).
// NOTE: holds RLock across marshal + disk I/O. Fine while Save is only called
// from CrawlAndPersist (infrequent, user-triggered). If concurrent or periodic
// saves are added, snapshot state under lock and write outside it.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := make([]StoredNode, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, *n)
	}

	doc := document{
		Version: schemaVersion,
		Nodes:   nodes,
		Edges:   s.edges,
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

// Merge integrates a crawled graph into the store. Nodes are upserted with
// fresh timestamps. Edges are deduplicated. The etags map provides etag values
// keyed by URL. Returns the number of nodes upserted.
func (s *Store) Merge(g *graph.Graph, etags map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	count := 0

	for _, n := range g.AllNodes() {
		sn := &StoredNode{
			URL:       n.URL,
			Title:     n.Title,
			Status:    n.Status,
			LinkCount: n.LinkCount,
			CrawledAt: now,
		}
		if etags != nil {
			sn.Etag = etags[n.URL]
		}
		s.nodes[n.URL] = sn
		count++
	}

	for _, e := range g.GetEdges() {
		se := StoredEdge{From: e.From, To: e.To}
		if _, exists := s.edgeSet[se]; !exists {
			s.edgeSet[se] = struct{}{}
			s.edges = append(s.edges, se)
		}
	}

	return count
}

// Backlinks returns all URLs that link to the given URL, sorted alphabetically.
func (s *Store) Backlinks(url string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []string
	for _, e := range s.edges {
		if e.To == url {
			result = append(result, e.From)
		}
	}
	sort.Strings(result)
	return result
}

// BacklinkEntry is a backlink with enriched node metadata.
type BacklinkEntry struct {
	URL    string
	Title  string
	Status string
}

// BacklinksEnriched returns backlinks for the given URL, enriched with
// title and status from the store. Sorted alphabetically by URL.
func (s *Store) BacklinksEnriched(url string) []BacklinkEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var urls []string
	for _, e := range s.edges {
		if e.To == url {
			urls = append(urls, e.From)
		}
	}
	sort.Strings(urls)

	entries := make([]BacklinkEntry, 0, len(urls))
	for _, u := range urls {
		entry := BacklinkEntry{URL: u}
		if n := s.nodes[u]; n != nil {
			entry.Title = n.Title
			entry.Status = n.Status
		}
		entries = append(entries, entry)
	}
	return entries
}

// InDegrees returns a map of URL to inbound edge count for all nodes.
// This is an efficient single-pass computation over edges.
func (s *Store) InDegrees() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[string]int, len(s.nodes))
	for url := range s.nodes {
		counts[url] = 0
	}
	for _, e := range s.edges {
		counts[e.To]++
	}
	return counts
}

// AllNodes returns a copy of all stored nodes.
func (s *Store) AllNodes() []StoredNode {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := make([]StoredNode, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, *n)
	}
	return nodes
}

// GetNode returns the stored node for a URL, or nil if not found.
func (s *Store) GetNode(url string) *StoredNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := s.nodes[url]
	if n == nil {
		return nil
	}
	cp := *n
	return &cp
}

// ToGraph reconstructs an in-memory graph.Graph from the stored state.
// All nodes have Depth 0 since depth is a crawl-session concept.
func (s *Store) ToGraph() *graph.Graph {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g := graph.New()
	for _, n := range s.nodes {
		g.AddNode(&graph.Node{
			URL:       n.URL,
			Title:     n.Title,
			Status:    n.Status,
			LinkCount: n.LinkCount,
		})
	}
	for _, e := range s.edges {
		g.AddEdge(e.From, e.To)
	}
	return g
}

// EtagFetcher wraps a fetch function that returns (status, body, etag, error)
// and implements graph.Fetcher while collecting etags concurrently.
// Use Etags() to retrieve the collected etags after crawling.
type EtagFetcher struct {
	fetchFunc func(host, path string) (status, body, etag string, err error)
	mu        sync.Mutex
	etags     map[string]string
}

// NewEtagFetcher creates a fetcher that collects etags during crawl.
// The fetchFunc should return (status, body, etag, error) for each document.
func NewEtagFetcher(fetchFunc func(host, path string) (status, body, etag string, err error)) *EtagFetcher {
	return &EtagFetcher{
		fetchFunc: fetchFunc,
		etags:     make(map[string]string),
	}
}

// Fetch implements graph.Fetcher.
func (f *EtagFetcher) Fetch(host, path string) (graph.FetchResult, error) {
	status, body, etag, err := f.fetchFunc(host, path)
	if err != nil {
		return graph.FetchResult{}, err
	}
	if etag != "" {
		url := "mark://" + host + path
		f.mu.Lock()
		f.etags[url] = etag
		f.mu.Unlock()
	}
	return graph.FetchResult{Status: status, Body: body}, nil
}

// Etags returns the collected etags keyed by URL.
func (f *EtagFetcher) Etags() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(f.etags))
	maps.Copy(cp, f.etags)
	return cp
}

// CrawlOptions configures a persistent crawl.
type CrawlOptions struct {
	MaxDepth int               // max link hops from start (0 = start node only, -1 = default 2)
	MaxNodes int               // node cap (0 = unlimited)
	Workers  int               // concurrent workers (0 = default 5)
	OnNode   func(*graph.Node) // optional per-node callback
}

// CrawlAndPersist runs a graph crawl, merges results into the store, and saves.
// If the store is nil, the crawl runs but results are not persisted.
// The fetchFunc should return (status, body, etag, error) for each document.
// The parseURL function parses mark:// URLs into (host, path, error).
// Returns the crawled graph.
func (s *Store) CrawlAndPersist(
	ctx context.Context,
	startURL string,
	fetchFunc func(host, path string) (status, body, etag string, err error),
	parseURL func(string) (string, string, error),
	opts CrawlOptions,
) (*graph.Graph, error) {
	fetcher := NewEtagFetcher(fetchFunc)

	var nodeCount atomic.Int32
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, err := graph.Crawl(ctx, startURL, fetcher, parseURL, graph.CrawlOptions{
		MaxDepth: opts.MaxDepth,
		Workers:  opts.Workers,
		OnNode: func(n *graph.Node) {
			if opts.OnNode != nil {
				opts.OnNode(n)
			}
			if opts.MaxNodes > 0 {
				if int(nodeCount.Add(1)) >= opts.MaxNodes {
					cancel()
				}
			}
		},
	})
	if err != nil {
		return g, err
	}

	if s != nil {
		s.Merge(g, fetcher.Etags())
		if saveErr := s.Save(); saveErr != nil {
			return g, saveErr
		}
	}

	return g, nil
}

// NodeCount returns the number of stored nodes.
func (s *Store) NodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.nodes)
}

// EdgeCount returns the number of stored edges.
func (s *Store) EdgeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.edges)
}
