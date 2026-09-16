package graphstore

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

type sourceRecord struct {
	Node  StoredNode   `json:"node"`
	Edges []StoredEdge `json:"edges"`
}

// ObserveDocument merges one ordinary fetch without indexing generated graph
// artifacts. Complete observations replace adjacency; failures age freshness.
func (s *Store) ObserveDocument(docURL string, result graph.FetchResult) bool {
	docURL = links.CanonicalURL(docURL)
	if generatedGraphURL(docURL) {
		return false
	}
	source := result.Source
	if source == "" {
		source = docURL
	}
	node := &graph.Node{URL: docURL, Status: result.Status, Observation: graph.Observe(source, result.Metadata)}
	node.Observation.Complete = graph.SourceComplete(node)
	representation := hashDocumentRepresentation(result.Body, result.Metadata)
	if s.refreshEqualDocument(node, representation) {
		return true
	}
	var extracted graph.ExtractedEdges
	if result.Status == protocol.StatusOK {
		extracted = graph.ExtractDocumentEdges(docURL, result.Body, result.Metadata)
		node.Title = links.ExtractTitle(result.Body)
		node.LinkCount = extracted.BodyLinkCount
	}
	if !node.Observation.Complete {
		node.Observation.Problem = "incomplete"
	}

	observed := graph.New()
	observed.AddNode(node)
	for _, edge := range extracted.Edges {
		observed.AddEdgeInfo(edge)
	}
	s.Merge(observed, nil)
	s.rememberDocumentRepresentation(node, extracted, representation)
	return true
}

func (s *Store) refreshEqualDocument(incoming *graph.Node, representation [sha256.Size]byte) bool {
	next := &incoming.Observation
	if incoming.Status != protocol.StatusOK || next.Etag == "" || !next.Known() {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	selected := s.nodes[incoming.URL]
	local, exists := s.localSources[incoming.URL]
	cachedRepresentation, cached := s.representations[incoming.URL]
	if !exists || !cached || cachedRepresentation != representation || selected == nil || selected.Seeded || local.Node.Seeded ||
		!sameDocumentRepresentation(&local.Node, incoming) || !sameDocumentRepresentation(selected, incoming) ||
		local.Node.Observation.Problem != "" || selected.Observation.Problem != "" ||
		local.Node.Observation.HighestRevision > next.Revision || selected.Observation.HighestRevision > next.Revision ||
		s.sourceRevisions[next.Source] > next.Revision ||
		next.ObservedAt.Before(local.Node.Observation.ObservedAt) || next.ObservedAt.Before(selected.Observation.ObservedAt) {
		return false
	}

	now := time.Now().UTC()
	local.Node.Observation = *next
	local.Node.CrawledAt = now
	s.localSources[incoming.URL] = local
	selected.Observation = *next
	selected.CrawledAt = now
	return true
}

func (s *Store) rememberDocumentRepresentation(node *graph.Node, extracted graph.ExtractedEdges, representation [sha256.Size]byte) {
	expected := sourceRecord{
		Node: StoredNode{
			URL: node.URL, Title: node.Title, Status: node.Status, LinkCount: node.LinkCount,
			Observation: node.Observation, Etag: node.Observation.Etag,
		},
		Edges: make([]StoredEdge, 0, len(extracted.Edges)),
	}
	for _, edge := range extracted.Edges {
		expected.Edges = append(expected.Edges, StoredEdge{
			From: edge.From, To: edge.To, Rel: edge.Rel, Label: edge.Label, Anchor: edge.Anchor, Count: max(edge.Count, 1),
		})
	}
	slices.SortFunc(expected.Edges, compareSnapshotEdges)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.representations == nil {
		s.representations = make(map[string][sha256.Size]byte)
	}
	accepted, exists := s.localSources[node.URL]
	if !exists || accepted.Node.Title != node.Title || accepted.Node.LinkCount != node.LinkCount ||
		accepted.Node.Observation.Problem != "" || !sameDocumentRepresentation(&accepted.Node, node) || !sameAdjacency(&accepted, &expected) {
		delete(s.representations, node.URL)
		return
	}
	s.representations[node.URL] = representation
}

func hashDocumentRepresentation(body string, metadata map[string]string) [sha256.Size]byte {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if strings.HasPrefix(key, "rel-") {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	digest := sha256.New()
	var length [8]byte
	var scratch [1024]byte
	writeField := func(value string) {
		binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:]) // hash.Hash.Write cannot fail.
		for value != "" {
			n := copy(scratch[:], value)
			_, _ = digest.Write(scratch[:n]) // hash.Hash.Write cannot fail.
			value = value[n:]
		}
	}
	writeField(body)
	for _, key := range keys {
		writeField(key)
		writeField(metadata[key])
	}
	var sum [sha256.Size]byte
	return [sha256.Size]byte(digest.Sum(sum[:0]))
}

func sameDocumentRepresentation(current *StoredNode, incoming *graph.Node) bool {
	old, next := &current.Observation, &incoming.Observation
	return current.Status == incoming.Status && current.Etag == next.Etag && old.Known() &&
		old.Source == next.Source && old.View == next.View && old.Revision == next.Revision && old.Etag == next.Etag
}

func generatedGraphURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	shards := SnapshotShardRoot(SnapshotManifestPath) + "/"
	return generatedGraphPath(parsed.Path, "/graph.md") || generatedGraphPath(parsed.Path, SnapshotManifestPath) || strings.HasPrefix(parsed.Path, shards)
}

func generatedGraphPath(path, base string) bool {
	if path == base {
		return true
	}
	version, ok := strings.CutPrefix(path, base+"/v")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(version)
	return err == nil && n > 0
}

func sourceRecords(nodes []StoredNode, edges []StoredEdge) map[string]sourceRecord { //nolint:gocritic // normalize private candidate copies
	records := make(map[string]sourceRecord, len(nodes))
	for i := range nodes {
		node := nodes[i]
		node.URL = links.CanonicalURL(node.URL)
		if node.Observation.Source != "" {
			node.Observation.Source = links.CanonicalURL(node.Observation.Source)
		}
		records[node.URL] = sourceRecord{Node: node}
	}
	for _, edge := range edges {
		edge.From, edge.To = links.CanonicalURL(edge.From), links.CanonicalURL(edge.To)
		edge.Count = max(edge.Count, 1)
		record := records[edge.From]
		if record.Node.URL == "" {
			record.Node.URL = edge.From
		}
		record.Edges = append(record.Edges, edge)
		records[edge.From] = record
	}
	for key := range records {
		record := records[key]
		slices.SortFunc(record.Edges, compareSnapshotEdges)
	}
	return records
}

func recordsData(records map[string]sourceRecord) seedGraphData {
	var data seedGraphData
	for _, key := range slices.Sorted(maps.Keys(records)) {
		record := records[key]
		data.Nodes = append(data.Nodes, record.Node)
		data.Edges = append(data.Edges, record.Edges...)
	}
	return data
}

func sameAdjacency(a, b *sourceRecord) bool {
	return a.Node.Status == b.Node.Status && slices.Equal(a.Edges, b.Edges)
}

func completeRecord(record *sourceRecord) bool {
	return observedStatus(record.Node.Status) && (record.Node.Observation.Source == "" || (record.Node.Observation.Complete && record.Node.Observation.KnownView()))
}

// Revalidation may resolve operational status without a new immutable version.
// Seed timestamps never order observations or archive state.
func reconcileObservation(previous, incoming sourceRecord, direct bool) sourceRecord { //nolint:gocritic // candidates must remain independent of their stored owners
	old, next := previous.Node.Observation, incoming.Node.Observation
	if graph.CompareRevision(&next, &old) == "older" {
		if direct {
			previous.Node.Observation.Problem = "revision-regression"
			previous.Node.Observation.AttemptedAt = next.AttemptedAt
		}
		return previous
	}
	if !completeRecord(&incoming) {
		previous.Node.Observation.Problem = "incomplete"
		previous.Node.Observation.AttemptedAt = next.AttemptedAt
		retainPartialEdges(&previous, &incoming)
		return previous
	}
	order := graph.CompareRevision(&next, &old)
	if order == "newer" {
		return incoming
	}
	if order == "equal" && differentViews(&previous, &incoming) {
		if next.View == graph.ViewDocument {
			return incoming
		}
		return previous
	}
	if direct && next.Source != "" && (old.Source == "" || old.Source == next.Source) {
		if next.ObservedAt.Before(old.ObservedAt) {
			return previous
		}
		if incoming.Node.Status == "not-found" || incoming.Node.Status == "archived" {
			incoming.Node.Observation.Revision = max(next.Revision, old.Revision)
			if next.Etag == "" {
				incoming.Node.Observation.Etag = old.Etag
			}
			return incoming
		}
		if !old.Known() || (order == "equal" && (sameAdjacency(&previous, &incoming) || previous.Node.Status != incoming.Node.Status)) {
			return incoming
		}
	}
	if order == "equal" && sameAdjacency(&previous, &incoming) {
		refreshValidation(&previous.Node.Observation, &next)
		return previous
	}
	if old.Source == "" && next.Source == "" {
		return incoming
	}
	previous.Node.Observation.Problem = "revision-conflict"
	previous.Node.Observation.AttemptedAt = next.AttemptedAt
	return previous
}

func retainPartialEdges(previous, incoming *sourceRecord) {
	seen := make(map[edgeKey]bool, len(previous.Edges))
	for _, edge := range previous.Edges {
		seen[edge.key()] = true
	}
	previous.Edges = slices.Clone(previous.Edges)
	for _, edge := range incoming.Edges {
		if !seen[edge.key()] {
			previous.Edges = append(previous.Edges, edge)
			seen[edge.key()] = true
		}
	}
	slices.SortFunc(previous.Edges, compareSnapshotEdges)
}

func differentViews(a, b *sourceRecord) bool {
	return a.Node.Status == b.Node.Status && a.Node.Observation.KnownView() && b.Node.Observation.KnownView() && a.Node.Observation.View != b.Node.Observation.View
}

func refreshValidation(current, next *graph.Observation) {
	if next.ObservedAt.After(current.ObservedAt) && !next.ObservedAt.After(time.Now()) {
		*current = *next
	}
}

func selectSources(local map[string]sourceRecord, owners map[string]map[string]sourceRecord) map[string]sourceRecord { //nolint:gocritic // selection annotates copies without mutating owner evidence
	selected := make(map[string]sourceRecord)
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		records := owners[owner]
		for key := range records {
			candidate := records[key]
			previous, exists := selected[key]
			if !exists {
				selected[key] = candidate
				continue
			}
			selected[key] = chooseSource(previous, candidate)
		}
	}
	for key := range local {
		candidate := local[key]
		previous, exists := selected[key]
		if !exists {
			selected[key] = candidate
			continue
		}
		// Local is the tie-break incumbent; a newer seed still wins.
		chosen := chooseSource(candidate, previous)
		if chosen.Node.Seeded {
			chosen.Node.CrawledAt = candidate.Node.CrawledAt
		}
		selected[key] = chosen
	}
	return selected
}

func chooseSource(incumbent, candidate sourceRecord) sourceRecord { //nolint:gocritic // selection annotates only the returned copy
	if !completeRecord(&incumbent) && completeRecord(&candidate) {
		return candidate
	}
	if !completeRecord(&candidate) {
		return incumbent
	}
	old, next := incumbent.Node.Observation, candidate.Node.Observation
	if !old.Known() && next.Known() && (old.Source == "" || old.Source == next.Source) {
		return candidate
	}
	if !incumbent.Node.Seeded && old.Known() && next.Revision == 0 && (next.Source == "" || next.Source == old.Source) {
		return incumbent
	}
	order := graph.CompareRevision(&candidate.Node.Observation, &incumbent.Node.Observation)
	if !incumbent.Node.Seeded && order == "equal" && incumbent.Node.Status != candidate.Node.Status {
		return incumbent
	}
	if order == "newer" {
		return candidate
	}
	if order == "older" {
		return incumbent
	}
	if order == "equal" && differentViews(&incumbent, &candidate) {
		if next.View == graph.ViewDocument {
			return candidate
		}
		return incumbent
	}
	if order == "equal" && sameAdjacency(&incumbent, &candidate) {
		refreshValidation(&incumbent.Node.Observation, &next)
		return incumbent
	}
	if order != "equal" || !sameAdjacency(&incumbent, &candidate) {
		incumbent.Node.Observation.Problem = "revision-conflict"
	}
	return incumbent
}

// MarkSeedFailure discloses failed refresh without deleting owned observations.
func (s *Store) MarkSeedFailure(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := s.seedGraphs[owner]
	for key := range records {
		record := records[key]
		record.Node.Observation.Problem = "seed-refresh-failed"
		records[key] = record
	}
	s.rebuildSeedsLocked()
}

// FreshnessSummaryFor counts only the given sources, so a card reports its own
// backlinks rather than the whole store. Unknown URLs count as unknown.
func (s *Store) FreshnessSummaryFor(urls []string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	counts := map[string]int{}
	for _, url := range urls {
		var observation *graph.Observation
		if node := s.nodes[links.CanonicalURL(url)]; node != nil {
			observation = &node.Observation
		}
		counts[observation.Freshness()]++
	}
	return fmt.Sprintf("freshness: %d fresh, %d stale, %d unknown", counts["fresh"], counts["stale"], counts["unknown"])
}

// Keep time-dependent validation separate from deterministic revision selection.
func dueObservation(node *StoredNode, now time.Time) bool {
	return node.Observation.AttemptedAt.IsZero() || node.Observation.AttemptedAt.After(now) || now.Sub(node.Observation.AttemptedAt) >= graph.FreshnessWindow
}
