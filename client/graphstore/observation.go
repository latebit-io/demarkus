package graphstore

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
)

type sourceRecord struct {
	Node  StoredNode   `json:"node"`
	Edges []StoredEdge `json:"edges"`
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

func sourceRecordsFromStore(nodes map[string]*StoredNode, edges []StoredEdge) map[string]sourceRecord {
	records := make(map[string]sourceRecord, len(nodes))
	for key, node := range nodes {
		records[key] = sourceRecord{Node: *node}
	}
	for _, edge := range edges {
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

func selectSources(local map[string]sourceRecord, owners map[string]seedGraphData) map[string]sourceRecord { //nolint:gocritic // selection annotates copies without mutating owner evidence
	selected := make(map[string]sourceRecord)
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		data := owners[owner]
		records := sourceRecords(data.Nodes, data.Edges)
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
	data := s.seedGraphs[owner]
	for i := range data.Nodes {
		data.Nodes[i].Observation.Problem = "seed-refresh-failed"
	}
	s.seedGraphs[owner] = data
	s.rebuildSeedsLocked()
}

// FreshnessSummary includes unknown coverage even when no backlinks remain.
func (s *Store) FreshnessSummary() string {
	counts := map[string]int{}
	nodes := s.AllNodes()
	for i := range nodes {
		counts[nodes[i].Observation.Freshness()]++
	}
	return fmt.Sprintf("freshness: %d fresh, %d stale, %d unknown", counts["fresh"], counts["stale"], counts["unknown"])
}

// Keep time-dependent validation separate from deterministic revision selection.
func dueObservation(node *StoredNode, now time.Time) bool {
	return node.Observation.AttemptedAt.IsZero() || node.Observation.AttemptedAt.After(now) || now.Sub(node.Observation.AttemptedAt) >= graph.FreshnessWindow
}
