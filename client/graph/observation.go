package graph

import (
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/links"
)

// FreshnessWindow bounds claims of recent source validation, not source changes.
const FreshnessWindow = 5 * time.Minute

// Extraction views preserve the ordinary and federation target policies.
const (
	ViewDocument   = "document"
	ViewFederation = "federation"
)

// Observation describes source evidence, never the snapshot carrying it.
// Problem and AttemptedAt can advance without replacing last-good evidence.
type Observation struct {
	Source          string    `json:"source,omitempty"`
	View            string    `json:"view,omitempty"`
	Revision        int       `json:"revision,omitempty"`
	HighestRevision int       `json:"highest_revision,omitempty"`
	Etag            string    `json:"etag,omitempty"`
	Complete        bool      `json:"complete,omitempty"`
	ObservedAt      time.Time `json:"observed_at,omitzero"`
	AttemptedAt     time.Time `json:"attempted_at,omitzero"`
	Problem         string    `json:"problem,omitempty"`
}

// Observe captures a FETCH revision, including metadata-only writes.
func Observe(source string, metadata map[string]string) Observation {
	if !strings.HasPrefix(source, "mark://") {
		return Observation{}
	}
	revision, err := strconv.Atoi(metadata["version"])
	if err != nil || revision < 1 {
		revision = 0 // Generated indexes and legacy peers have no document revision.
	}
	now := time.Now().UTC()
	return Observation{Source: links.CanonicalURL(source), View: ViewDocument, Revision: revision,
		Etag: metadata["etag"], ObservedAt: now, AttemptedAt: now}
}

// Known requires a complete, revision-bearing source observation.
func (o *Observation) Known() bool {
	return o != nil && o.Source != "" && o.Revision > 0 && o.Complete && o.KnownView() && !o.ObservedAt.IsZero()
}

// KnownView distinguishes complete projections from unspecified extraction.
func (o *Observation) KnownView() bool {
	return o != nil && (o.View == ViewDocument || o.View == ViewFederation)
}

// Freshness qualifies evidence by validation age and unresolved attempts.
func (o *Observation) Freshness() string {
	if !o.Known() {
		return "unknown"
	}
	if o.HighestRevision > o.Revision || o.Problem != "" || time.Since(o.ObservedAt) > FreshnessWindow || o.ObservedAt.After(time.Now()) {
		return "stale"
	}
	return "fresh"
}

// Annotation is shared by graph, backlink and topology consumers.
func (o *Observation) Annotation() string {
	if o == nil {
		return " [freshness: unknown]"
	}
	s := " [freshness: " + o.Freshness()
	if o.View == ViewFederation {
		s += "; view: federation"
	}
	if o.Revision > 0 {
		s += "; source-v" + strconv.Itoa(o.Revision)
	}
	if o.HighestRevision > o.Revision {
		s += "; seen-v" + strconv.Itoa(o.HighestRevision)
	}
	if o.Problem != "" {
		s += "; " + o.Problem
	}
	return s + "]"
}

// CompareRevision returns older, equal, newer, or incomparable for one source.
// Etags establish equality only; they are not lexicographically ordered clocks.
func CompareRevision(a, b *Observation) string {
	if a.Source == "" || a.Source != b.Source || a.Revision < 1 || b.Revision < 1 {
		return "incomparable"
	}
	if a.Revision < b.Revision {
		return "older"
	}
	if a.Revision > b.Revision {
		return "newer"
	}
	if a.Etag != b.Etag {
		return "incomparable"
	}
	return "equal"
}
