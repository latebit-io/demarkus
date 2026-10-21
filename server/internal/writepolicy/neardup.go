package writepolicy

import (
	"context"
	"errors"
	"slices"

	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// nearDuplicates judges the body before the commit on its own view: the
// precondition's reader has no catalog, and a view inside the commit is the
// deadlock Read warns about. A duplicate landing in between is only a missed warning.
func (e *enforcer) nearDuplicates(ctx context.Context, req backend.WriteRequest) (notes []string) {
	draft := neardup.AnalyzeWhole(string(req.Content))
	if draft.Sig == 0 {
		return nil
	}
	view, err := e.OpenReadView(ctx)
	if err != nil {
		return []string{"near-duplicate check skipped: " + err.Error()}
	}
	defer func() {
		if err := view.Close(); err != nil {
			notes = append(notes, "near-duplicate check: close view: "+err.Error())
		}
	}()
	own := storefmt.CanonicalPath(req.Path)
	// Stale: the newest index built, never a wait for one covering this
	// snapshot; a warning may trail a commit.
	rows, err := view.Lookup(ctx, "", catalog.Options{Match: catalog.MatchSimilar, Draft: &draft, Stale: true, Skip: func(path string) bool { return path == own }})
	switch {
	// No section index yet: no check, as body match falls back to the catalog.
	case errors.Is(err, backend.ErrBodyMatchUnavailable):
		return notes
	case err != nil:
		return append(notes, "near-duplicate check skipped: "+err.Error())
	case len(rows) == 0:
		return notes
	}
	// The template is read only with hits in hand: what repeats by design
	// is neither judged nor named.
	lay, err := currentLayout(ctx, view)
	if err != nil {
		return append(notes, "near-duplicate check skipped: "+err.Error())
	}
	if lay.RepeatsByDesign(own) {
		return notes
	}
	hits := make([]neardup.Hit, 0, len(rows))
	for i := range rows {
		if !lay.RepeatsByDesign(rows[i].Path) {
			hits = append(hits, rows[i].Hit)
		}
	}
	if note := neardup.Note(slices.Clip(hits)); note != "" {
		notes = append(notes, note)
	}
	return notes
}

// currentLayout is the world's template through reader, the default when
// there is none or it is archived; a stored one parses, the gate refused the others.
func currentLayout(ctx context.Context, reader documentGetter) (layout.Layout, error) {
	document, found, err := currentDocument(ctx, reader, layout.DocumentPath)
	if err != nil || !found {
		return layout.Default(), err
	}
	return layout.Parse(string(document.Content))
}
