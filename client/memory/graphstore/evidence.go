package graphstore

import (
	"slices"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/graph"
)

// sourceEvidence is the occurrence index of one locally observed source at
// one revision: each link and rel- value with its fragment and section.
// Derived from the body and never seeded; no wire format carries it.
type sourceEvidence struct {
	Revision    int                `json:"revision"`
	Etag        string             `json:"etag,omitempty"`
	Occurrences []graph.Occurrence `json:"occurrences"`
}

// current reports whether the evidence describes the node as selected now.
func (e *sourceEvidence) current(node *StoredNode) bool {
	return node != nil && !node.Seeded && node.Observation.Revision == e.Revision && node.Observation.Etag == e.Etag
}

// recordEvidence keeps the occurrences of a complete document-view read the
// store accepted cleanly as the local record. A rejected read (older revision,
// conflict) keeps earlier evidence while it matches the accepted revision.
func (s *Store) recordEvidence(node *graph.Node, occurrences []graph.Occurrence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	observation := &node.Observation
	accepted, exists := s.localSources[node.URL]
	if !exists {
		delete(s.evidence, node.URL)
		return
	}
	selected := &accepted.Node.Observation
	if node.Status != "ok" || !observation.Complete || observation.View != graph.ViewDocument ||
		selected.Problem != "" || selected.Revision != observation.Revision || selected.Etag != observation.Etag {
		if current, ok := s.evidence[node.URL]; !ok || current.Revision != selected.Revision || current.Etag != selected.Etag {
			delete(s.evidence, node.URL)
		}
		return
	}
	if s.evidence == nil {
		s.evidence = make(map[string]sourceEvidence)
	}
	s.evidence[node.URL] = sourceEvidence{Revision: observation.Revision, Etag: observation.Etag, Occurrences: slices.Clone(occurrences)}
}

// pruneEvidenceLocked drops the evidence of each url whose local record
// moved on or is gone; only a merged source can have.
func (s *Store) pruneEvidenceLocked(urls []string) {
	for _, url := range urls {
		e, held := s.evidence[url]
		if !held {
			continue
		}
		if accepted, exists := s.localSources[url]; !exists || accepted.Node.Observation.Revision != e.Revision || accepted.Node.Observation.Etag != e.Etag {
			delete(s.evidence, url)
		}
	}
}

// edgeOccurrencesLocked is the current evidence behind one selected edge.
func (s *Store) edgeOccurrencesLocked(edge *StoredEdge, source *StoredNode) []graph.Occurrence {
	e, ok := s.evidence[edge.From]
	if !ok || !e.current(source) {
		return nil
	}
	var out []graph.Occurrence
	for i := range e.Occurrences {
		if o := &e.Occurrences[i]; o.To == edge.To && o.Rel == edge.Rel {
			out = append(out, *o)
		}
	}
	return out
}

func (s *Store) loadEvidence(doc *document) {
	for url, e := range doc.Evidence {
		url = links.CanonicalURL(url)
		if accepted, exists := s.localSources[url]; exists && accepted.Node.Observation.Revision == e.Revision && accepted.Node.Observation.Etag == e.Etag {
			if s.evidence == nil {
				s.evidence = make(map[string]sourceEvidence)
			}
			s.evidence[url] = e
		}
	}
}
