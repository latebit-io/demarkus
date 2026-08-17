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

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/internal/tokens"
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
// Identity is {From, To, Rel}; the enrichment fields are optional so
// pre-enrichment graph.json files load unchanged (schema stays v1).
type StoredEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Rel    string `json:"rel,omitempty"`
	Label  string `json:"label,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// edgeKey is the identity of a stored edge for deduplication.
type edgeKey struct{ from, to, rel string }

func (e *StoredEdge) key() edgeKey {
	return edgeKey{from: e.From, to: e.To, rel: e.Rel}
}

// document is the on-disk JSON envelope. SeedEtags is additive (omitted when
// empty) so pre-seed graph.json files round-trip unchanged; schema stays v1.
type document struct {
	Version   int               `json:"version"`
	Nodes     []StoredNode      `json:"nodes"`
	Edges     []StoredEdge      `json:"edges"`
	SeedEtags map[string]string `json:"seed_etags,omitempty"`
}

// Store is the persistent graph state.
type Store struct {
	path      string
	mu        sync.RWMutex
	nodes     map[string]*StoredNode
	edges     []StoredEdge
	edgeIdx   map[edgeKey]int
	seedEtags map[string]string // host -> last seen /graph.md etag
}

// DefaultPath returns the default graph store location (~/.mark/graph.json).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "graph.json")
}

// New returns an empty in-memory Store. No filesystem path is
// associated, so Save() is a no-op — useful for callers that want
// the graph data structure without the disk-backing (e.g. the
// broker MCP gateway, which keeps a per-pod ephemeral store).
// Load(path) is the right constructor for the CLI/TUI which
// wants the disk-backing.
func New() *Store {
	return &Store{
		nodes:     make(map[string]*StoredNode),
		edgeIdx:   make(map[edgeKey]int),
		seedEtags: make(map[string]string),
	}
}

// Load reads a graph store from disk. Returns an empty store if
// the file does not exist. Returns an error for other I/O or
// parse failures.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("graphstore: empty path")
	}

	s := &Store{
		path:      path,
		nodes:     make(map[string]*StoredNode),
		edgeIdx:   make(map[edgeKey]int),
		seedEtags: make(map[string]string),
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
		if e.Count < 1 {
			e.Count = 1 // legacy rows predate Count; an edge on disk means one occurrence
		}
		if _, exists := s.edgeIdx[e.key()]; !exists {
			s.edgeIdx[e.key()] = len(s.edges)
			s.edges = append(s.edges, e)
		}
	}
	maps.Copy(s.seedEtags, doc.SeedEtags)

	return s, nil
}

// Save writes the graph store to disk atomically (write tmp, rename).
// When s.path is empty (in-memory store from New()) this is a
// no-op so callers can treat persistence as transparent and rely
// on the New()-constructed store for ephemeral use cases.
// NOTE: holds RLock across marshal + disk I/O. Fine while Save is only called
// from CrawlAndPersist (infrequent, user-triggered). If concurrent or periodic
// saves are added, snapshot state under lock and write outside it.
func (s *Store) Save() error {
	if s.path == "" {
		// In-memory store; nothing to persist. CrawlAndPersist
		// still calls us unconditionally, so this no-op is the
		// load-bearing part of the New() contract.
		return nil
	}
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
	if len(s.seedEtags) > 0 {
		doc.SeedEtags = s.seedEtags
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

// Merge integrates a crawled graph into the store; returns nodes upserted.
// Stored nodes and their outgoing edge sets are replaced only for sources
// the crawl actually read: an unread node (error/external/blank) never
// regresses last-known-good data or drops edges it knows nothing about,
// though never-seen unread nodes are still recorded.
func (s *Store) Merge(g *graph.Graph, etags map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	count := 0

	// Sources whose full outgoing set this crawl observed. "external" and
	// "error" nodes were not read, so their stored edges are kept; a fetched
	// not-found/archived doc has no links and its old edges rightly drop.
	refreshed := make(map[string]bool)

	for _, n := range g.AllNodes() {
		if !observedStatus(n.Status) {
			// Failed fetch: we learned nothing about this node. Keep the
			// stored copy untouched (its CrawledAt keeps it authoritative
			// against seeds); only record nodes we have never seen.
			if s.nodes[n.URL] == nil {
				s.nodes[n.URL] = &StoredNode{
					URL:       n.URL,
					Title:     n.Title,
					Status:    n.Status,
					LinkCount: n.LinkCount,
					CrawledAt: now,
				}
				count++
			}
			continue
		}
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
		refreshed[n.URL] = true
	}

	s.dropEdgesFromLocked(refreshed)

	for _, e := range g.GetEdges() {
		se := StoredEdge{From: e.From, To: e.To, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)}
		s.upsertEdgeLocked(&se)
	}

	return count
}

// dropEdgesFromLocked removes every stored edge whose From is in sources and
// rebuilds the index. Caller must hold s.mu.
func (s *Store) dropEdgesFromLocked(sources map[string]bool) {
	if len(sources) == 0 {
		return
	}
	kept := make([]StoredEdge, 0, len(s.edges))
	for _, e := range s.edges {
		if !sources[e.From] {
			kept = append(kept, e)
		}
	}
	if len(kept) != len(s.edges) {
		s.edges = kept
		s.edgeIdx = make(map[edgeKey]int, len(kept))
		for i := range kept {
			s.edgeIdx[kept[i].key()] = i
		}
	}
}

// upsertEdgeLocked inserts or replaces an edge by identity. Last write wins,
// mirroring node upserts: re-merging the same crawl is idempotent and counts
// never inflate. Caller must hold s.mu.
func (s *Store) upsertEdgeLocked(se *StoredEdge) {
	if i, exists := s.edgeIdx[se.key()]; exists {
		s.edges[i] = *se
	} else {
		s.edgeIdx[se.key()] = len(s.edges)
		s.edges = append(s.edges, *se)
	}
}

// observedStatus reports whether status means the document was actually
// read, so its recorded outgoing edge set is complete. "external" and
// "error" nodes were not read; their edge sets carry no information.
func observedStatus(status string) bool {
	return status != "" && status != "external" && status != "error"
}

// authoritativeLocked reports whether the stored node for url reflects a real
// local crawl: a genuinely fetched status and a non-zero CrawledAt. Seeded
// nodes carry zero CrawledAt, the durable "not locally observed" marker.
// Caller must hold s.mu.
func (s *Store) authoritativeLocked(url string) bool {
	n := s.nodes[url]
	if n == nil {
		return false
	}
	return observedStatus(n.Status) && !n.CrawledAt.IsZero()
}

// SeedFromExport merges a published /graph.md aggregate (as parsed by
// ParseExport) into the store. Local wins: nodes and edges of a locally
// authoritative source are never touched. Seeded nodes get zero CrawledAt so
// a later local crawl flips them authoritative through Merge. For every
// non-authoritative source in the seed, the stored outgoing edge set is
// replaced, so stale rows from an earlier seed do not linger. Returns the
// number of nodes seeded.
func (s *Store) SeedFromExport(nodes []StoredNode, edges []StoredEdge) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	added := 0
	for _, n := range nodes {
		if s.authoritativeLocked(n.URL) {
			continue
		}
		sn := n
		sn.CrawledAt = time.Time{}
		sn.Etag = "" // the hub export carries no per-node etags
		s.nodes[sn.URL] = &sn
		added++
	}

	// Sources whose outgoing edge set this seed replaces: every source the
	// aggregate actually observed (real status) plus every seed-edge From,
	// excluding locally authoritative ones. Node-based inclusion matters
	// when a source's outgoing set went to zero: no edge rows remain, yet
	// its stale seeded edges must still drop (Merge's refreshed criterion,
	// applied to the seed's view).
	seeded := make(map[string]bool)
	for _, n := range nodes {
		if observedStatus(n.Status) && !s.authoritativeLocked(n.URL) {
			seeded[n.URL] = true
		}
	}
	for _, e := range edges {
		if !s.authoritativeLocked(e.From) {
			seeded[e.From] = true
		}
	}
	s.dropEdgesFromLocked(seeded)

	for _, e := range edges {
		if !seeded[e.From] {
			continue
		}
		e.Count = max(e.Count, 1)
		s.upsertEdgeLocked(&e)
	}

	return added
}

// SeedEtag returns the last recorded /graph.md etag for host ("" if none).
func (s *Store) SeedEtag(host string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seedEtags[host]
}

// SetSeedEtag records the /graph.md etag for host, persisted by Save.
func (s *Store) SetSeedEtag(host, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seedEtags[host] = etag
}

// Backlinks returns all URLs that link to the given URL, sorted alphabetically.
// A source appears once even when it carries several edges (plain plus typed).
func (s *Store) Backlinks(url string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]bool)
	var result []string
	for _, e := range s.edges {
		if e.To == url && !seen[e.From] {
			seen[e.From] = true
			result = append(result, e.From)
		}
	}
	sort.Strings(result)
	return result
}

// BacklinkEntry is a backlink with enriched node and edge metadata.
type BacklinkEntry struct {
	URL    string
	Title  string
	Status string
	Rel    string // typed-relation predicate ("" for plain body links)
	Label  string // link label text
	Anchor string // source section anchor (no '#')
	Count  int    // occurrences of this edge
}

// BacklinksEnriched returns backlinks for the given URL, enriched with node
// title/status and edge provenance. One entry per edge, so a source appears
// once per relation. Sorted by URL then Rel.
func (s *Store) BacklinksEnriched(url string) []BacklinkEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []BacklinkEntry
	for _, e := range s.edges {
		if e.To != url {
			continue
		}
		entry := BacklinkEntry{URL: e.From, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)}
		if n := s.nodes[e.From]; n != nil {
			entry.Title = n.Title
			entry.Status = n.Status
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].URL != entries[j].URL {
			return entries[i].URL < entries[j].URL
		}
		return entries[i].Rel < entries[j].Rel
	})
	return entries
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
		g.AddEdgeInfo(graph.Edge{From: e.From, To: e.To, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)})
	}
	return g
}

// FetchFunc fetches a document and returns its status, body, and publisher
// metadata; the etag rides in Metadata["etag"].
type FetchFunc func(host, path string) (graph.FetchResult, error)

// EtagFetcher wraps a FetchFunc and implements graph.Fetcher while collecting
// etags concurrently. Use Etags() to retrieve them after crawling.
type EtagFetcher struct {
	fetchFunc FetchFunc
	mu        sync.Mutex
	etags     map[string]string
}

// NewEtagFetcher creates a fetcher that collects etags during crawl.
func NewEtagFetcher(fetchFunc FetchFunc) *EtagFetcher {
	return &EtagFetcher{
		fetchFunc: fetchFunc,
		etags:     make(map[string]string),
	}
}

// Fetch implements graph.Fetcher.
func (f *EtagFetcher) Fetch(host, path string) (graph.FetchResult, error) {
	res, err := f.fetchFunc(host, path)
	if err != nil {
		return graph.FetchResult{}, err
	}
	if etag := res.Metadata["etag"]; etag != "" {
		url := "mark://" + host + path
		f.mu.Lock()
		f.etags[url] = etag
		f.mu.Unlock()
	}
	return res, nil
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

// NewFetchFunc creates a FetchFunc for CrawlAndPersist from a protocol
// client and token store, resolving tokens per host.
func NewFetchFunc(client *fetch.Client, tokenStore *tokens.Store) FetchFunc {
	return func(host, path string) (graph.FetchResult, error) {
		r, fetchErr := client.Fetch(host, path, tokens.Resolve("", host, tokenStore))
		if fetchErr != nil {
			return graph.FetchResult{}, fetchErr
		}
		return graph.FetchResult{
			Status:   r.Response.Status,
			Body:     r.Response.Body,
			Metadata: r.Response.Metadata,
		}, nil
	}
}

// CrawlAndPersist crawls from startURL, merges the result into the store, and
// saves; a nil store crawls without persisting. parseURL splits mark:// URLs
// and canonicalizes startURL, so any address form the user typed is accepted.
func (s *Store) CrawlAndPersist(
	ctx context.Context,
	startURL string,
	fetchFunc FetchFunc,
	parseURL func(string) (string, string, error),
	opts CrawlOptions,
) (*graph.Graph, error) {
	// Canonical mark://host:port/path is the node-identity contract shared by
	// stored keys, the etag map, and /graph.md. A parse error means a non-mark://
	// start, which Crawl records as external, so pass it through untouched.
	if host, path, parseErr := parseURL(startURL); parseErr == nil {
		startURL = "mark://" + host + path
	}

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
