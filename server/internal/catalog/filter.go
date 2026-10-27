package catalog

import (
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// predKind distinguishes the supported filter predicate forms.
type predKind int

const (
	predExact     predKind = iota // declared metadata key equals value
	predTag                       // membership in the document's tags (key "tag")
	predModAfter                  // modification time after a date
	predModBefore                 // modification time before a date
	predBasedOn                   // a rel-based-on pin on a source, any version
)

// Predicate is one parsed filter clause. A document must satisfy every
// predicate in a filter to be included in results.
type Predicate struct {
	kind  predKind
	key   string    // predExact: the metadata key
	value string    // predExact, predTag: the value to compare; predBasedOn: the source
	t     time.Time // predModAfter, predModBefore: the boundary
}

// ParseFilter parses comma-separated "key=value" clauses. Built-ins: "tag"
// (membership), "modified-after"/"modified-before" (against the modification
// time), "based-on" (BasedOn); any other key matches metadata exactly.
func ParseFilter(s string) ([]Predicate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var preds []Predicate
	for clause := range strings.SplitSeq(s, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		key, value, ok := strings.Cut(clause, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("malformed filter clause %q", clause)
		}
		switch key {
		case "modified-after", "modified-before":
			t, err := parseFilterTime(value)
			if err != nil {
				return nil, fmt.Errorf("filter %s: %w", key, err)
			}
			kind := predModAfter
			if key == "modified-before" {
				kind = predModBefore
			}
			preds = append(preds, Predicate{kind: kind, t: t})
		case "tag":
			preds = append(preds, Predicate{kind: predTag, value: value})
		case "based-on":
			preds = append(preds, BasedOn(value))
		default:
			preds = append(preds, Predicate{kind: predExact, key: key, value: value})
		}
	}
	return preds, nil
}

// BasedOn matches documents pinning source, or anything under it, at any
// version: a document path, a directory, a repository URL.
func BasedOn(source string) Predicate {
	if !strings.Contains(source, "://") {
		source = storefmt.CanonicalPath(source)
	}
	return Predicate{kind: predBasedOn, value: source}
}

// parseFilterTime accepts an RFC 3339 timestamp or a bare date (YYYY-MM-DD).
func parseFilterTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid date %q (want RFC 3339 or YYYY-MM-DD)", s)
}

// MatchesAll reports whether the entry satisfies every predicate. Exported
// so LookupCatalog backends apply identical filter semantics; Predicate
// fields stay unexported on purpose.
func MatchesAll(e *Entry, preds []Predicate) bool {
	for _, p := range preds {
		if !p.matches(e) {
			return false
		}
	}
	return true
}

// matches reports whether a single predicate holds for the entry.
func (p Predicate) matches(e *Entry) bool {
	switch p.kind {
	case predExact:
		return e.Metadata[p.key] == p.value
	case predTag:
		for _, t := range e.Tags {
			if strings.EqualFold(t, p.value) {
				return true
			}
		}
		return false
	case predModAfter:
		return e.Modified.After(p.t)
	case predModBefore:
		return e.Modified.Before(p.t)
	case predBasedOn:
		for _, pin := range provenance.Parse(e.Metadata[provenance.Key]) {
			if pin.Under(e.Path, p.value) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
