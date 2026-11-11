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
	"github.com/latebit-io/demarkus/client/internal/filelock"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/graph"
)

// schemaVersion is the on-disk format version. Increment on breaking changes.
// v4 retains local candidates separately from revision-selected seed observations.
const schemaVersion = 4

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
	// GoneAt is when a local read first found the document absent or
	// archived; the record is dropped once nothing needs it past deadRetention.
	GoneAt time.Time `json:"gone_at,omitzero"`
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
	// Evidence is derived and schema-neutral: an older binary drops it on save.
	Evidence map[string]sourceEvidence `json:"evidence,omitempty"`
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
	worldPins       map[string]*WorldManifest          // owner -> world checkpoint its seed holds the shards of
	localSources    map[string]sourceRecord
	representations map[string][sha256.Size]byte
	validating      map[string]time.Time
	sourceRevisions map[string]int
	evidence        map[string]sourceEvidence // local source URL -> occurrences at its accepted revision
	// free lists the slots of edges a patched source left empty; a rebuild
	// compacts them away.
	free []int
	// sourceRefs counts the selected nodes per observation source, so a
	// source's high water leaves with the last node that shows it.
	sourceRefs map[string]int
	lastSweep  time.Time
	// disk is the file as this store last read or wrote it (saveMu);
	// seedDirty names the owners whose seed this process replaced since.
	disk      fileStamp
	seedDirty map[string]bool
}

// A dead local source (not found, archived) stays while a seed claims the
// document, something links to it, or a stale seed may still arrive: past
// deadRetention it is dropped, checked every sweepInterval.
const (
	deadRetention = 7 * 24 * time.Hour
	sweepInterval = time.Hour
)

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
		worldPins:       make(map[string]*WorldManifest),
		localSources:    make(map[string]sourceRecord),
		representations: make(map[string][sha256.Size]byte),
		sourceRefs:      make(map[string]int),
		seedDirty:       make(map[string]bool),
	}
}

// Load reads a graph store from disk. Returns an empty store if
// the file does not exist. Returns an error for other I/O or
// parse failures.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("graphstore: empty path")
	}
	s := New()
	s.path = path
	doc, stamp, err := readDocument(path)
	if err != nil {
		return nil, err
	}
	s.disk = stamp
	if doc != nil {
		s.fill(doc)
	}
	return s, nil
}

// fileStamp tells one saved file from the next without reading it.
type fileStamp struct {
	modified time.Time
	size     int64
}

// readDocument reads the store file: nil when there is none, or when an
// older schema wrote it (a crawl cache is dropped, not upgraded, and the
// next Save writes the current schema).
func readDocument(path string) (*document, fileStamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fileStamp{}, nil
	}
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read graph store %q: %w", path, err)
	}
	stamp := fileStamp{modified: info.ModTime(), size: info.Size()}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fileStamp{}, fmt.Errorf("read graph store %q: %w", path, err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fileStamp{}, fmt.Errorf("parse graph store %q: %w", path, err)
	}
	if doc.Version < 1 || doc.Version > schemaVersion {
		// A missing version is a damaged file, not an older schema: refuse
		// rather than let the next Save overwrite it.
		return nil, fileStamp{}, fmt.Errorf("graph store %q: unsupported schema version %d (expected %d)", path, doc.Version, schemaVersion)
	}
	if doc.Version < schemaVersion {
		return nil, stamp, nil
	}
	return &doc, stamp, nil
}

// fill loads doc into an empty store the caller alone holds.
func (s *Store) fill(doc *document) {
	s.nodes = make(map[string]*StoredNode, len(doc.Nodes))
	s.edges = make([]StoredEdge, 0, len(doc.Edges))
	s.edgeIdx = make(map[edgeKey]int, len(doc.Edges))

	// Rows load as stored: every write path canonicalizes URLs before Save.
	// Generated graph data an older version stored is left behind.
	for i := range doc.Nodes {
		n := doc.Nodes[i]
		if !graph.IsGeneratedURL(n.URL) {
			s.nodes[n.URL] = &n
		}
	}
	for _, e := range doc.Edges {
		if graph.IsGeneratedURL(e.From) || graph.IsGeneratedURL(e.To) {
			continue
		}
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
	s.loadCandidates(doc)
	s.sourceRevisions = doc.SourceRevisions
	s.loadEvidence(doc)
	for _, node := range s.nodes {
		s.sourceRefs[node.Observation.Source]++
	}
	// A file from before GoneAt starts its dead sources' clock now.
	now := time.Now().UTC()
	for key := range s.localSources {
		if record := s.localSources[key]; gone(record.Node.Status) && record.Node.GoneAt.IsZero() {
			record.Node.GoneAt = now
			s.localSources[key] = record
		}
	}
	s.patchLocked(s.sweepDeadLocked(now))
}

// loadCandidates rebuilds local (unseeded) sources; seeded rows are owned
// by seedGraphs and skipped without building a record.
func (s *Store) loadCandidates(doc *document) {
	for key, node := range s.nodes {
		if !node.Seeded {
			s.localSources[key], _ = s.selectedRecordLocked(key)
		}
	}
	for key := range s.outgoing {
		if s.nodes[key] == nil {
			s.localSources[key], _ = s.selectedRecordLocked(key)
		}
	}
	for key := range doc.LocalSources {
		record := doc.LocalSources[key]
		normalized := sourceRecords([]StoredNode{record.Node}, record.Edges)
		maps.Copy(s.localSources, normalized)
	}
}

// Save atomically persists one store snapshot. Several processes share the
// file (MCP servers, the TUI, the CLI), so under its lock a save first takes
// in what another wrote since this store last read it: nobody's work is lost.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return filelock.With(s.path, func() error {
		if err := s.mergeDisk(); err != nil {
			return err
		}
		data, err := json.MarshalIndent(s.saveDocument(), "", "  ")
		if err != nil {
			return err
		}
		if err := writeSaveFile(s.path, data); err != nil {
			return err
		}
		return s.saved()
	})
}

// saved records the file as this store's own: nothing in it is news.
func (s *Store) saved() error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("stat graph store %q: %w", s.path, err)
	}
	s.disk = fileStamp{modified: info.ModTime(), size: info.Size()}
	s.mu.Lock()
	clear(s.seedDirty)
	s.mu.Unlock()
	return nil
}

// mergeDisk takes in the file when another process saved it since this
// store read or wrote it. A file this store cannot read is not overwritten.
// Callers hold saveMu and the file lock.
func (s *Store) mergeDisk() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat graph store %q: %w", s.path, err)
	}
	if (fileStamp{modified: info.ModTime(), size: info.Size()}) == s.disk {
		return nil
	}
	doc, _, err := readDocument(s.path)
	if err != nil || doc == nil {
		return err
	}
	other := New()
	other.fill(doc)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.takeLocked(other)
	return nil
}

// takeLocked merges another process's store: a local source it read at a
// newer revision, or later, replaces this one's; a seed this process did
// not replace itself is taken at the other's generation.
func (s *Store) takeLocked(other *Store) {
	changed := false
	for key := range other.localSources {
		theirs := other.localSources[key]
		if ours, held := s.localSources[key]; held && !readLater(&theirs.Node, &ours.Node) {
			continue
		}
		s.localSources[key], changed = theirs, true
		delete(s.representations, key)
		delete(s.evidence, key)
		if evidence, held := other.evidence[key]; held {
			if s.evidence == nil {
				s.evidence = make(map[string]sourceEvidence)
			}
			s.evidence[key] = evidence
		}
	}
	for owner, records := range other.seedGraphs {
		if _, held := s.seedGraphs[owner]; s.seedDirty[owner] || (held && s.seedEtags[owner] == other.seedEtags[owner]) {
			continue
		}
		s.seedGraphs[owner], s.seedEtags[owner], changed = records, other.seedEtags[owner], true
		// The pins named the shards of the seed just replaced.
		delete(s.worldPins, owner)
	}
	if !changed {
		return
	}
	if s.sourceRevisions == nil {
		s.sourceRevisions = make(map[string]int)
	}
	for source, revision := range other.sourceRevisions {
		s.sourceRevisions[source] = max(s.sourceRevisions[source], revision)
	}
	s.rebuildSeedsLocked()
	s.patchLocked(s.sweepDeadLocked(time.Now().UTC()))
}

// readLater reports theirs as the better record of one source: a newer
// revision, else the later read.
func readLater(theirs, ours *StoredNode) bool {
	switch graph.CompareRevision(&theirs.Observation, &ours.Observation) {
	case "newer":
		return true
	case "older":
		return false
	}
	return theirs.CrawledAt.After(ours.CrawledAt)
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
		Edges:           s.liveEdgesLocked(),
		SourceRevisions: maps.Clone(s.sourceRevisions),
	}
	if len(s.evidence) > 0 {
		doc.Evidence = maps.Clone(s.evidence) // values are replaced whole, never appended to
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
// without regressing stored rows or their last-known-good outgoing sets. Only
// the sources g names are reselected: the cost is theirs, not the store's.
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
	keys := make([]string, 0, len(records))
	for key := range records {
		s.localSources[key] = s.acceptLocked(key, records[key], now)
		keys = append(keys, key)
	}
	s.pruneEvidenceLocked(keys)
	if now.Sub(s.lastSweep) >= sweepInterval {
		s.lastSweep = now
		keys = append(keys, s.sweepDeadLocked(now)...)
	}
	s.patchLocked(keys)
	return len(nodes)
}

// acceptLocked is the local record of key once incoming, one read of it, is
// weighed against what the store holds.
func (s *Store) acceptLocked(key string, incoming sourceRecord, now time.Time) sourceRecord { //nolint:gocritic // the record is reconciled into a new one
	delete(s.representations, key)
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
	incoming.Node.GoneAt = time.Time{}
	if gone(incoming.Node.Status) {
		incoming.Node.GoneAt = now
		if held := s.localSources[key].Node; gone(held.Status) && !held.GoneAt.IsZero() {
			incoming.Node.GoneAt = held.GoneAt
		}
	}
	return incoming
}

// gone is a status that confirms the document is not there to read.
func gone(status string) bool { return status == "not-found" || status == "archived" }

// Sweep drops the dead local sources nothing needs as of now and reports
// how many: Merge does it every sweepInterval, Load once.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := s.sweepDeadLocked(now)
	s.patchLocked(dropped)
	return len(dropped)
}

// sweepDeadLocked forgets each local source dead for deadRetention that no
// seed claims and nothing links to; the caller reselects the keys it returns.
func (s *Store) sweepDeadLocked(now time.Time) (dropped []string) {
	for key := range s.localSources {
		node := s.localSources[key].Node
		if !gone(node.Status) || node.GoneAt.IsZero() || now.Sub(node.GoneAt) < deadRetention || len(s.incoming[key]) > 0 || s.seedOwnsLocked(key) {
			continue
		}
		delete(s.localSources, key)
		delete(s.evidence, key)
		delete(s.representations, key)
		delete(s.validating, key)
		dropped = append(dropped, key)
	}
	return dropped
}

func (s *Store) seedOwnsLocked(key string) bool {
	for _, records := range s.seedGraphs {
		if _, owned := records[key]; owned {
			return true
		}
	}
	return false
}

// patchLocked reselects keys alone: each one's node and edges are replaced
// in place, leaving every other source as it is.
func (s *Store) patchLocked(keys []string) {
	if len(keys) == 0 {
		return
	}
	if s.sourceRevisions == nil {
		s.sourceRevisions = make(map[string]int)
	}
	order := slices.Sorted(maps.Keys(s.seedGraphs))
	for _, key := range keys {
		candidate, shown := selectSource(key, s.localSources, s.seedGraphs, order)
		if shown {
			s.annotateLocked(&candidate)
		}
		// Counted in before the old node is counted out, so a source both
		// show keeps its high water.
		if old := s.nodes[key]; old != nil {
			s.unrefLocked(old.Observation.Source)
		}
		s.dropEdgesLocked(key)
		if !shown {
			delete(s.nodes, key)
			continue
		}
		node := candidate.Node
		s.nodes[key] = &node
		s.addEdgesLocked(candidate.Edges)
	}
	// Mostly empty slots: the flat edge list is rebuilt to its live size.
	if len(s.free) > 1024 && len(s.free) > len(s.edges)/2 {
		s.rebuildSeedsLocked()
	}
}

// annotateLocked counts candidate's source in, raises its high water and
// marks a selected revision under it.
func (s *Store) annotateLocked(candidate *sourceRecord) {
	observation := &candidate.Node.Observation
	s.sourceRefs[observation.Source]++
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
}

// unrefLocked counts one node of source out. A source no seed or crawl
// holds any more is forgotten with its high water, or every source ever
// seen would stay.
func (s *Store) unrefLocked(source string) {
	s.sourceRefs[source]--
	if s.sourceRefs[source] <= 0 {
		delete(s.sourceRefs, source)
		delete(s.sourceRevisions, source)
	}
}

// dropEdgesLocked removes key's outgoing edges, leaving their slots free.
func (s *Store) dropEdgesLocked(key string) {
	for _, index := range s.outgoing[key] {
		edge := &s.edges[index]
		delete(s.edgeIdx, edge.key())
		if rest := slices.DeleteFunc(s.incoming[edge.To], func(i int) bool { return i == index }); len(rest) > 0 {
			s.incoming[edge.To] = rest
		} else {
			delete(s.incoming, edge.To)
		}
		*edge = StoredEdge{}
		s.free = append(s.free, index)
	}
	delete(s.outgoing, key)
}

// addEdgesLocked indexes one source's edges, into free slots first.
func (s *Store) addEdgesLocked(edges []StoredEdge) {
	for i := range edges {
		edge := &edges[i]
		if index, exists := s.edgeIdx[edge.key()]; exists {
			s.edges[index] = *edge
			continue
		}
		index := len(s.edges)
		if n := len(s.free); n > 0 {
			index, s.free = s.free[n-1], s.free[:n-1]
			s.edges[index] = *edge
		} else {
			s.edges = append(s.edges, *edge)
		}
		s.edgeIdx[edge.key()] = index
		s.incoming[edge.To] = append(s.incoming[edge.To], index)
		s.outgoing[edge.From] = append(s.outgoing[edge.From], index)
	}
}

// liveEdgesLocked copies the edges, the free slots left out.
func (s *Store) liveEdgesLocked() []StoredEdge {
	edges := make([]StoredEdge, 0, len(s.edges)-len(s.free))
	for i := range s.edges {
		if s.edges[i].From != "" {
			edges = append(edges, s.edges[i])
		}
	}
	return edges
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
	return s.replaceSeed(owner, &seedUpdate{nodes: nodes, edges: edges})
}

// replaceSeed makes u's rows, plus the current records u keeps, the owner's
// seed, and u's pins the checkpoint it holds.
func (s *Store) replaceSeed(owner string, u *seedUpdate) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.seedGraphs[owner]
	nodes := u.nodes
	incoming := sourceRecords(nodes, u.edges)
	if u.keep != nil {
		for key := range old {
			if _, replaced := incoming[key]; !replaced && u.keep(key) {
				incoming[key] = old[key]
			}
		}
	}
	s.setPinsLocked(owner, u.pins)
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
	s.seedDirty[owner] = true
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

func (s *Store) setPinsLocked(owner string, pins *WorldManifest) {
	if pins == nil {
		delete(s.worldPins, owner)
		return
	}
	s.worldPins[owner] = pins
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

// rebuildSeedsLocked reselects every source: what a seed replacement costs,
// and what compacts the edge list.
func (s *Store) rebuildSeedsLocked() {
	selected := selectSources(s.localSources, s.seedGraphs)
	if s.sourceRevisions == nil {
		s.sourceRevisions = make(map[string]int)
	}
	s.sourceRefs = make(map[string]int, len(selected))
	s.nodes = make(map[string]*StoredNode, len(selected))
	edgeCapacity := 0
	for key := range selected {
		edgeCapacity += len(selected[key].Edges)
	}
	s.edges = make([]StoredEdge, 0, edgeCapacity)
	s.edgeIdx = make(map[edgeKey]int, edgeCapacity)
	s.free = nil
	for _, key := range slices.Sorted(maps.Keys(selected)) {
		record := selected[key]
		s.annotateLocked(&record)
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
	maps.DeleteFunc(s.sourceRevisions, func(source string, _ int) bool { return s.sourceRefs[source] == 0 })
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
		if e.From == "" {
			continue // a free slot
		}
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
	return len(s.edges) - len(s.free)
}
