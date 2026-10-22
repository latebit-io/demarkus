package writepolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// ErrNearDuplicate reports a write refused because the world already holds
// its body in other words.
var ErrNearDuplicate = errors.New("near-duplicate check refused mutation")

// NearDuplicateError carries the hits so the handler can name them.
type NearDuplicateError struct {
	Hits []neardup.Hit
}

func (err *NearDuplicateError) Error() string {
	return fmt.Sprintf("near-duplicate check: %d document(s) already hold this", len(err.Hits))
}

func (err *NearDuplicateError) Unwrap() []error {
	return []error{ErrNearDuplicate, backend.ErrRejected}
}

// RejectionMessage names the documents and the way out: update or append to
// one of them, or republish with the override reason to keep both.
func (err *NearDuplicateError) RejectionMessage() string {
	parts := make([]string, len(err.Hits))
	for i, h := range err.Hits {
		parts[i] = h.String()
	}
	return "near-duplicate check refused this write: " + strings.Join(parts, ", ") + "\nFix: update or append to that document instead; to keep both, republish with metadata " + neardup.OverrideKey + " set to the reason.\n"
}

// nearDuplicates judges the body before the commit on its own view: the
// precondition's reader has no catalog, and a view inside the commit is the
// deadlock Read warns about. A duplicate landing in between is only a missed warning.
func (e *enforcer) nearDuplicates(ctx context.Context, req backend.WriteRequest) (notes []string, err error) {
	mode := e.options.Content.NearDuplicate
	if mode == contentcheck.Off {
		return nil, nil
	}
	if reason := strings.TrimSpace(req.Metadata[neardup.OverrideKey]); reason != "" {
		return []string{"near-duplicate check overridden (" + reason + ")"}, nil
	}
	draft := neardup.AnalyzeWhole(string(req.Content))
	if draft.Sig == 0 {
		return nil, nil
	}
	view, err := e.OpenReadView(ctx)
	if err != nil {
		return []string{"near-duplicate check skipped: " + err.Error()}, nil
	}
	defer func() {
		if err := view.Close(); err != nil {
			notes = append(notes, "near-duplicate check: close view: "+err.Error())
		}
	}()
	own := storefmt.CanonicalPath(req.Path)
	rows, err := view.Lookup(ctx, "", catalog.Options{Match: catalog.MatchSimilar, Draft: &draft, Stale: true, Skip: func(path string) bool { return path == own }})
	switch {
	// No section index within the bound: no check, as body match falls
	// back to the catalog.
	case errors.Is(err, backend.ErrBodyMatchUnavailable):
		return notes, nil
	case err != nil:
		return append(notes, "near-duplicate check skipped: "+err.Error()), nil
	case len(rows) == 0:
		return notes, nil
	}
	// The template is read only with hits in hand: what repeats by design
	// is neither judged nor named.
	lay, err := currentLayout(ctx, view)
	if err != nil {
		return append(notes, "near-duplicate check skipped: "+err.Error()), nil
	}
	if lay.RepeatsByDesign(own) {
		return notes, nil
	}
	hits := make([]neardup.Hit, 0, len(rows))
	for i := range rows {
		if !lay.RepeatsByDesign(rows[i].Path) {
			hits = append(hits, rows[i].Hit)
		}
	}
	if len(hits) == 0 {
		return notes, nil
	}
	if mode == contentcheck.Warn {
		return append(notes, neardup.Note(slices.Clip(hits))), nil
	}
	return nil, &NearDuplicateError{Hits: hits}
}

// currentLayout is the world's template through reader, the default when
// there is none or it is archived; a stored one parses, the gate refused the others.
func currentLayout(ctx context.Context, reader documentGetter) (layout.Layout, error) {
	document, found, err := currentDocument(ctx, reader, layout.DocumentPath)
	if err != nil || !found || document.Archived {
		return layout.Default(), err
	}
	return layout.Parse(string(document.Content))
}
