package audit

import (
	"context"
	"time"

	"github.com/latebit-io/demarkus/client/memory/promote"
)

// PromotedDoc is one live document the inventory found promoted: by its
// markers, or by the tag alone when Markers.Target is empty.
type PromotedDoc struct {
	Path    string
	Markers promote.Markers
}

// Inventory is Promoted's result. Coverage lists every read that failed or
// was cut short, so an inventory is complete only when it is empty.
type Inventory struct {
	Docs      []PromotedDoc
	Documents int // live documents listed
	Coverage  []string
}

// Promoted inventories the promoted documents under scope over the doctor's
// walk and bounds, so a refresh needs no capped tag lookup. Documents come
// in document order.
func Promoted(ctx context.Context, store Store, scope string, timeout time.Duration) (*Inventory, error) {
	a, err := newAudit(store, Options{Scope: scope, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	if err := a.inventory(ctx); err != nil {
		return nil, err
	}
	if err := a.fetchAll(ctx); err != nil {
		return nil, err
	}
	inv := &Inventory{Documents: len(a.order), Coverage: a.coverage}
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		if doc, ok := Classify(p, d.meta, d.body); ok {
			inv.Docs = append(inv.Docs, doc)
		}
	}
	return inv, nil
}

// Classify is one live document's inventory entry: its markers when it is
// promoted, by marker or by tag alone, and false when it is not.
func Classify(path string, meta map[string]string, body string) (PromotedDoc, bool) {
	if !promote.IsPromoted(meta, body) {
		return PromotedDoc{}, false
	}
	markers, _ := promote.ParseMarkers(body) // found is Target != ""
	return PromotedDoc{Path: path, Markers: markers}, true
}
