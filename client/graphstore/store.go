// Package graphstore provides persistent storage for the document link graph.
//
// The graph is stored as a JSON file at ~/.mark/graph.json. It records nodes
// (documents) and edges (links between documents) discovered by the graph
// crawler, along with etags and timestamps for incremental updates.
package graphstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
)

// schemaVersion is the on-disk format version. Increment on breaking changes.
// v4 retains local candidates separately from revision-selected seed observations.
const schemaVersion = 4

// minSchemaVersion is the oldest on-disk format still readable.
const minSchemaVersion = 1

// StoredNode is a graph node with persistence metadata.
type StoredNode struct {
	Observation graph.Observation `json:"observation,omitzero"`
	URL         string            `json:"url"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	LinkCount   int               `json:"link_count"`
	Etag        string            `json:"etag,omitempty"`
	CrawledAt   time.Time         `json:"crawled_at"`
	Seeded      bool              `json:"seeded,omitempty"`
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

// Local and seed-owner candidates persist independently of the selected graph.
type document struct {
	Version         int                      `json:"version"`
	Nodes           []StoredNode             `json:"nodes"`
	Edges           []StoredEdge             `json:"edges"`
	SeedEtags       map[string]string        `json:"seed_etags,omitempty"`
	SeedGraphs      map[string]seedGraphData `json:"seed_graphs,omitempty"`
	LocalSources    map[string]sourceRecord  `json:"local_sources,omitempty"`
	SourceRevisions map[string]int           `json:"source_revisions,omitempty"`
}

type seedGraphData struct {
	Nodes []StoredNode `json:"nodes"`
	Edges []StoredEdge `json:"edges"`
}

// Store is the persistent graph state.
type Store struct {
	// seedChecks single-flights and throttles seed passes per owner.
	seedChecks      SeedGate
	path            string
	mu              sync.RWMutex
	saveMu          sync.Mutex
	nodes           map[string]*StoredNode
	edges           []StoredEdge
	edgeIdx         map[edgeKey]int
	incoming        map[string][]int
	outgoing        map[string][]int
	seedEtags       map[string]string                  // host -> last seen published graph etag
	seedGraphs      map[string]map[string]sourceRecord // owner -> normalized complete non-authoritative graph
	localSources    map[string]sourceRecord
	representations map[string][sha256.Size]byte
	validating      map[string]time.Time
	sourceRevisions map[string]int
}

// DefaultPath returns the default graph store location (~/.mark/graph.json).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "graph.json")
}

// New creates an ephemeral store; Save is a no-op. Load enables persistence.
func New() *Store {
	return &Store{
		nodes:           make(map[string]*StoredNode),
		edgeIdx:         make(map[edgeKey]int),
		incoming:        make(map[string][]int),
		outgoing:        make(map[string][]int),
		seedEtags:       make(map[string]string),
		seedGraphs:      make(map[string]map[string]sourceRecord),
		localSources:    make(map[string]sourceRecord),
		representations: make(map[string][sha256.Size]byte),
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
		path:            path,
		nodes:           make(map[string]*StoredNode),
		edgeIdx:         make(map[edgeKey]int),
		incoming:        make(map[string][]int),
		outgoing:        make(map[string][]int),
		seedEtags:       make(map[string]string),
		seedGraphs:      make(map[string]map[string]sourceRecord),
		localSources:    make(map[string]sourceRecord),
		representations: make(map[string][sha256.Size]byte),
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
	if doc.Version < minSchemaVersion || doc.Version > schemaVersion {
		return nil, fmt.Errorf("graph store %q: unsupported schema version %d (expected %d..%d)", path, doc.Version, minSchemaVersion, schemaVersion)
	}
	s.nodes = make(map[string]*StoredNode, len(doc.Nodes))
	s.edges = make([]StoredEdge, 0, len(doc.Edges))
	s.edgeIdx = make(map[edgeKey]int, len(doc.Edges))

	// v1 keyed nodes by whatever the crawler was handed, so one document could
	// appear under both mark://host/x and mark://host:6309/x. Canonicalizing on
	// load merges those, keeping the more recently crawled copy.
	for i := range doc.Nodes {
		n := doc.Nodes[i]
		n.URL = links.CanonicalURL(n.URL)
		if n.CrawledAt.IsZero() {
			n.Seeded = true
		}
		if prev := s.nodes[n.URL]; prev != nil && prev.CrawledAt.After(n.CrawledAt) {
			continue
		}
		s.nodes[n.URL] = &n
	}
	for _, e := range doc.Edges {
		if e.Count < 1 {
			e.Count = 1 // legacy rows predate Count; an edge on disk means one occurrence
		}
		e.From, e.To = links.CanonicalURL(e.From), links.CanonicalURL(e.To)
		if _, exists := s.edgeIdx[e.key()]; !exists {
			s.edgeIdx[e.key()] = len(s.edges)
			s.edges = append(s.edges, e)
		}
	}
	s.rebuildAdjacencyLocked()
	maps.Copy(s.seedEtags, doc.SeedEtags)
	for owner, seeded := range doc.SeedGraphs {
		records := sourceRecords(seeded.Nodes, seeded.Edges)
		for key := range records {
			record := records[key]
			record.Node.CrawledAt = time.Time{}
			record.Node.Seeded = true
			records[key] = record
		}
		s.seedGraphs[owner] = records
	}
	if doc.Version < 3 && len(s.seedEtags) > 0 {
		// Pre-snapshot stores lack owner provenance; force one full refresh.
		clear(s.seedEtags)
	}
	s.loadCandidates(&doc)
	s.sourceRevisions = doc.SourceRevisions
	return s, nil
}

func (s *Store) loadCandidates(doc *document) {
	records := s.sourceRecordsLocked()
	for key := range records {
		record := records[key]
		if !record.Node.Seeded {
			if doc.Version < 4 {
				record.Node.Observation = graph.Observation{}
			}
			s.localSources[key] = record
		}
	}
	if doc.Version < 3 {
		legacySeeds := make(map[string]sourceRecord)
		for key := range records {
			record := records[key]
			if record.Node.Seeded {
				record.Node.Observation = graph.Observation{}
				legacySeeds[key] = record
			}
		}
		if len(legacySeeds) > 0 {
			s.seedGraphs[""] = legacySeeds
		}
	}
	if doc.Version >= 4 {
		for key := range doc.LocalSources {
			record := doc.LocalSources[key]
			normalized := sourceRecords([]StoredNode{record.Node}, record.Edges)
			maps.Copy(s.localSources, normalized)
		}
	}

}

// Save atomically persists one store snapshot. Calls on one Store serialize;
// independent process owners of the same path remain last-writer-wins.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	doc := s.saveDocument()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeSaveFile(s.path, data)
}

func writeSaveFile(path string, data []byte) (saveErr error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create graph store temp file: %w", err)
	}
	tmp := file.Name()
	closed := false
	defer func() {
		if !closed {
			if err := file.Close(); err != nil {
				saveErr = errors.Join(saveErr, fmt.Errorf("close graph store temp file: %w", err))
			}
		}
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			saveErr = errors.Join(saveErr, fmt.Errorf("remove graph store temp file: %w", err))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure graph store temp file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write graph store temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close graph store temp file: %w", err)
	}
	closed = true
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace graph store: %w", err)
	}
	return nil
}

func (s *Store) saveDocument() document {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := make([]StoredNode, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, *n)
	}

	doc := document{
		Version:         schemaVersion,
		Nodes:           nodes,
		Edges:           slices.Clone(s.edges),
		SourceRevisions: maps.Clone(s.sourceRevisions),
	}
	// Selected local rows already live in Nodes/Edges; only shadowed candidates
	// need a second representation to survive seed-owner withdrawal.
	for key := range s.localSources {
		if selected := s.nodes[key]; selected != nil && selected.Seeded {
			if doc.LocalSources == nil {
				doc.LocalSources = make(map[string]sourceRecord)
			}
			record := s.localSources[key]
			record.Edges = slices.Clone(record.Edges)
			doc.LocalSources[key] = record
		}
	}
	if len(s.seedEtags) > 0 {
		doc.SeedEtags = maps.Clone(s.seedEtags)
	}
	if len(s.seedGraphs) > 0 {
		doc.SeedGraphs = make(map[string]seedGraphData, len(s.seedGraphs))
		for owner, records := range s.seedGraphs {
			doc.SeedGraphs[owner] = recordsData(records)
		}
	}
	return doc
}

// Merge replaces only fully observed sources; partial observations add new edges
// without regressing stored rows or their last-known-good outgoing sets.
func (s *Store) Merge(g *graph.Graph, etags map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	nodes := make([]StoredNode, 0, g.NodeCount())
	for _, n := range g.AllNodes() {
		nodeURL := links.CanonicalURL(n.URL)
		node := StoredNode{URL: nodeURL, Title: n.Title, Status: n.Status,
			LinkCount: n.LinkCount, CrawledAt: now, Observation: n.Observation, Etag: n.Observation.Etag}
		if node.Etag == "" {
			node.Etag = etags[nodeURL]
		}
		if n.Incomplete {
			node.Status = "partial"
			node.Observation.Complete = false
		}
		nodes = append(nodes, node)
	}
	edges := make([]StoredEdge, 0, g.EdgeCount())
	for _, e := range g.GetEdges() {
		edges = append(edges, StoredEdge{From: e.From, To: e.To, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)})
	}
	records := sourceRecords(nodes, edges)
	for key := range records {
		delete(s.representations, key)
		incoming := records[key]
		if incoming.Node.CrawledAt.IsZero() {
			incoming.Node.CrawledAt = now
		}
		previous, exists := s.selectedRecordLocked(key)
		if !observedStatus(incoming.Node.Status) {
			// A failed source read cannot turn seed ownership into a local claim.
			previous, exists = s.localSources[key]
			for _, records := range s.seedGraphs {
				if record, owned := records[key]; owned {
					record.Node.Observation.Problem = "incomplete"
					record.Node.Observation.AttemptedAt = incoming.Node.Observation.AttemptedAt
					records[key] = record
				}
			}
		}
		if exists {
			incoming = reconcileObservation(previous, incoming, true)
		}
		if incoming.Node.CrawledAt.IsZero() {
			incoming.Node.CrawledAt = now
		}
		incoming.Node.Seeded = false
		s.localSources[key] = incoming
	}
	s.rebuildSeedsLocked()
	return len(nodes)
}

// Caller must hold s.mu or own the unpublished store.
func (s *Store) rebuildAdjacencyLocked() {
	incomingCounts := make(map[string]int)
	outgoingCounts := make(map[string]int)
	for i := range s.edges {
		incomingCounts[s.edges[i].To]++
		outgoingCounts[s.edges[i].From]++
	}

	incoming := make(map[string][]int, len(incomingCounts))
	outgoing := make(map[string][]int, len(outgoingCounts))
	indices := make([]int, 2*len(s.edges))
	offset := 0
	for endpoint, count := range incomingCounts {
		incoming[endpoint] = indices[offset : offset : offset+count]
		offset += count
	}
	for endpoint, count := range outgoingCounts {
		outgoing[endpoint] = indices[offset : offset : offset+count]
		offset += count
	}
	for i := range s.edges {
		edge := &s.edges[i]
		incoming[edge.To] = append(incoming[edge.To], i)
		outgoing[edge.From] = append(outgoing[edge.From], i)
	}
	s.incoming = incoming
	s.outgoing = outgoing
}

// Only successful reads and confirmed absence establish a complete source.
func observedStatus(status string) bool {
	return graph.SourceComplete(&graph.Node{Status: status})
}

// SeedFromExport replaces the default owner's graph using source revisions.
func (s *Store) SeedFromExport(nodes []StoredNode, edges []StoredEdge) int {
	return s.ReplaceSeed("", nodes, edges)
}

// ReplaceSeed atomically replaces one owner's complete seeded graph.
// Candidate adjacency is selected whole; overlapping owners never union it.
func (s *Store) ReplaceSeed(owner string, nodes []StoredNode, edges []StoredEdge) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.seedGraphs[owner]
	incoming := sourceRecords(nodes, edges)
	for key := range incoming {
		record := incoming[key]
		if prior, exists := old[key]; exists {
			record = reconcileObservation(prior, record, false)
		}
		record.Node.CrawledAt = time.Time{}
		record.Node.Seeded = true
		incoming[key] = record
	}
	s.seedGraphs[owner] = incoming
	s.rebuildSeedsLocked()
	added := 0
	for i := range nodes {
		node := &nodes[i]
		if selected := s.nodes[links.CanonicalURL(node.URL)]; selected != nil && selected.Seeded {
			added++
		}
	}
	return added
}

// sourceRecordsLocked snapshots every selected source; Merge reads one key at a time instead.
func (s *Store) sourceRecordsLocked() map[string]sourceRecord {
	records := make(map[string]sourceRecord, len(s.nodes))
	for key := range s.nodes {
		records[key], _ = s.selectedRecordLocked(key)
	}
	for key := range s.outgoing {
		if _, exists := records[key]; !exists {
			records[key], _ = s.selectedRecordLocked(key)
		}
	}
	return records
}

// selectedRecordLocked reads one selected source without copying the whole store.
func (s *Store) selectedRecordLocked(key string) (sourceRecord, bool) {
	node := s.nodes[key]
	indices := s.outgoing[key]
	if node == nil && len(indices) == 0 {
		return sourceRecord{}, false
	}
	record := sourceRecord{Node: StoredNode{URL: key}}
	if node != nil {
		record.Node = *node
	}
	record.Edges = make([]StoredEdge, 0, len(indices))
	for _, index := range indices {
		record.Edges = append(record.Edges, s.edges[index])
	}
	slices.SortFunc(record.Edges, compareSnapshotEdges)
	return record, true
}

func (s *Store) rebuildSeedsLocked() {
	selected := selectSources(s.localSources, s.seedGraphs)
	if s.sourceRevisions == nil {
		s.sourceRevisions = make(map[string]int)
	}
	for key := range selected {
		candidate := selected[key]
		observation := &candidate.Node.Observation
		if observation.Source != "" && observation.HighestRevision > 0 {
			s.sourceRevisions[observation.Source] = max(s.sourceRevisions[observation.Source], observation.HighestRevision)
		}
		if observation.Known() {
			s.sourceRevisions[observation.Source] = max(s.sourceRevisions[observation.Source], observation.HighestRevision, observation.Revision)
		}
		if observation.Known() && observation.Revision < s.sourceRevisions[observation.Source] {
			observation.Problem = "revision-regression"
			observation.HighestRevision = s.sourceRevisions[observation.Source]
		}
		selected[key] = candidate
	}
	s.nodes = make(map[string]*StoredNode, len(selected))
	edgeCapacity := 0
	for key := range selected {
		edgeCapacity += len(selected[key].Edges)
	}
	s.edges = make([]StoredEdge, 0, edgeCapacity)
	s.edgeIdx = make(map[edgeKey]int, edgeCapacity)
	for _, key := range slices.Sorted(maps.Keys(selected)) {
		record := selected[key]
		node := record.Node
		s.nodes[key] = &node
		for i := range record.Edges {
			edge := &record.Edges[i]
			if index, exists := s.edgeIdx[edge.key()]; exists {
				s.edges[index] = *edge
				continue
			}
			s.edgeIdx[edge.key()] = len(s.edges)
			s.edges = append(s.edges, *edge)
		}
	}
	s.rebuildAdjacencyLocked()
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
	url = links.CanonicalURL(url)
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]bool)
	var result []string
	for _, index := range s.incoming[url] {
		e := &s.edges[index]
		if !seen[e.From] {
			seen[e.From] = true
			result = append(result, e.From)
		}
	}
	sort.Strings(result)
	return result
}

// BacklinkEntry is a backlink with enriched node and edge metadata.
type BacklinkEntry struct {
	Observation *graph.Observation
	URL         string
	Title       string
	Status      string
	Rel         string // typed-relation predicate ("" for plain body links)
	Label       string // link label text
	Anchor      string // source section anchor (no '#')
	Count       int    // occurrences of this edge
}

// BacklinksEnriched returns backlinks for the given URL, enriched with node
// title/status and edge provenance. One entry per edge, so a source appears
// once per relation. Sorted by URL then Rel.
func (s *Store) BacklinksEnriched(url string) []BacklinkEntry {
	url = links.CanonicalURL(url)
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]BacklinkEntry, 0, len(s.incoming[url]))
	for _, index := range s.incoming[url] {
		e := &s.edges[index]
		entry := BacklinkEntry{URL: e.From, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)}
		if n := s.nodes[e.From]; n != nil {
			entry.Title = n.Title
			entry.Status = n.Status
			if n.Observation != (graph.Observation{}) {
				observation := n.Observation
				entry.Observation = &observation
			}
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
	url = links.CanonicalURL(url)
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
			URL:         n.URL,
			Title:       n.Title,
			Status:      n.Status,
			LinkCount:   n.LinkCount,
			Observation: n.Observation,
		})
	}
	for _, e := range s.edges {
		g.AddEdgeInfo(graph.Edge{From: e.From, To: e.To, Rel: e.Rel, Label: e.Label, Anchor: e.Anchor, Count: max(e.Count, 1)})
	}
	return g
}

// FetchFunc fetches a document and returns its status, body, and publisher
// metadata; the etag rides in Metadata["etag"].
type FetchFunc func(ctx context.Context, target links.Target) (graph.FetchResult, error)

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
func (f *EtagFetcher) Fetch(ctx context.Context, target links.Target) (graph.FetchResult, error) {
	res, err := f.fetchFunc(ctx, target)
	if err != nil {
		return graph.FetchResult{}, err
	}
	if etag := res.Metadata["etag"]; etag != "" {
		url := target.NodeURL()
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

// CrawlOptions uses the same admission and resource limits as transient crawls.
type CrawlOptions = graph.CrawlOptions

// DocumentFetcher is the one verb a crawl needs; *fetch.Client is one.
type DocumentFetcher interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
}

// NewFetchFunc creates a FetchFunc for CrawlAndPersist that asks resolver for a
// token per dial host, so a credential reaches only the host it belongs to.
func NewFetchFunc(client DocumentFetcher, resolver fetch.TokenResolver) FetchFunc {
	return func(ctx context.Context, target links.Target) (graph.FetchResult, error) {
		host := target.DialHost()
		r, fetchErr := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: target.Path, Token: resolver.Token(host)})
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
// saves; a nil store crawls without persisting. Any address form of startURL
// is accepted; identity is canonicalized by the crawl.
func (s *Store) CrawlAndPersist(
	ctx context.Context,
	startURL string,
	fetchFunc FetchFunc,
	opts CrawlOptions,
) (*graph.Graph, error) {
	g, etags, err := crawlGraph(ctx, startURL, fetchFunc, opts)
	if s == nil || (err != nil && !errors.Is(err, graph.ErrIncomplete)) {
		return g, err
	}
	s.Merge(g, etags)
	if saveErr := s.Save(); saveErr != nil {
		return g, errors.Join(err, saveErr)
	}
	return g, err
}

// crawlGraph validates inputs and crawls; callers decide when to merge and save.
func crawlGraph(
	ctx context.Context,
	startURL string,
	fetchFunc FetchFunc,
	opts CrawlOptions,
) (*graph.Graph, map[string]string, error) {
	// Validate before wrapping a nil fetch function in a non-nil interface.
	if strings.HasPrefix(startURL, "mark://") && fetchFunc == nil {
		return nil, nil, fmt.Errorf("crawl %s: fetchFunc is required", startURL)
	}
	fetcher := NewEtagFetcher(fetchFunc)
	g, err := graph.Crawl(ctx, startURL, fetcher, opts)
	return g, fetcher.Etags(), err
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
