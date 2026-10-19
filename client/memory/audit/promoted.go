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

// Promoted inventories the promoted documents under scope over the doctor's
// walk and bounds, so a refresh needs no capped tag lookup. It returns them
// in document order with the number of live documents read.
func Promoted(ctx context.Context, store Store, scope string, timeout time.Duration) ([]PromotedDoc, int, error) {
	a, err := newAudit(store, Options{Scope: scope, Timeout: timeout})
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	if err := a.inventory(ctx); err != nil {
		return nil, 0, err
	}
	if err := a.fetchAll(ctx); err != nil {
		return nil, 0, err
	}
	var docs []PromotedDoc
	for _, p := range a.order {
		d := a.docs[p]
		if live(d.status) && promote.IsPromoted(d.meta, d.body) {
			markers, _ := promote.ParseMarkers(d.body) // found is Target != ""
			docs = append(docs, PromotedDoc{Path: p, Markers: markers})
		}
	}
	return docs, len(a.order), nil
}
