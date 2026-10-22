package writepolicy

import (
	"context"
	"errors"
	"fmt"
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

// nearVerdict is one judgment of a body against the world: the hits, the
// notes for the writer, and whether the index answered at all.
type nearVerdict struct {
	hits    []neardup.Hit
	notes   []string
	checked bool
}

// nearCatalog is the state a judgment reads: the section index and the
// world's template.
type nearCatalog interface {
	backend.CatalogReader
	documentGetter
}

// nearSubject is what a judgment reads of the write: the request's path,
// body and metadata, never the prepared merge of an append.
type nearSubject struct {
	path string
	body []byte
	meta map[string]string
}

func subjectOf(req backend.WriteRequest) nearSubject {
	return nearSubject{path: req.Path, body: req.Content, meta: req.Metadata}
}

// judgeNearDuplicate asks the index what the body repeats. An override
// lands with its reason noted; a cold index (ErrBodyMatchUnavailable) is a
// note and checked false; any other failure is the caller's to decide on.
func (e *enforcer) judgeNearDuplicate(ctx context.Context, state nearCatalog, subject nearSubject) (nearVerdict, error) {
	if reason := strings.TrimSpace(subject.meta[neardup.OverrideKey]); reason != "" {
		return nearVerdict{notes: []string{"near-duplicate check overridden (" + reason + ")"}, checked: true}, nil
	}
	draft := neardup.AnalyzeWhole(string(subject.body))
	if draft.Sig == 0 {
		return nearVerdict{checked: true}, nil
	}
	// A refusal reads the index covering its snapshot, waited for within
	// the freshness bound; a warning may trail a commit and reads the newest.
	own := storefmt.CanonicalPath(subject.path)
	stale := e.options.Content.NearDuplicate == contentcheck.Warn
	rows, err := state.Lookup(ctx, "", catalog.Options{Match: catalog.MatchSimilar, Draft: &draft, Stale: stale, Skip: func(p string) bool { return p == own }})
	switch {
	case errors.Is(err, backend.ErrBodyMatchUnavailable):
		return nearVerdict{notes: []string{"near-duplicate check skipped: no section index covers this write yet"}}, nil
	case err != nil:
		return nearVerdict{}, fmt.Errorf("near-duplicate check: %w", err)
	case len(rows) == 0:
		return nearVerdict{checked: true}, nil
	}
	// The template is read only with hits in hand: what repeats by design
	// is neither judged nor named.
	lay, err := currentLayout(ctx, state)
	if err != nil {
		return nearVerdict{}, fmt.Errorf("near-duplicate check: %w", err)
	}
	verdict := nearVerdict{checked: true}
	if lay.RepeatsByDesign(own) {
		return verdict, nil
	}
	for i := range rows {
		if !lay.RepeatsByDesign(rows[i].Path) {
			verdict.hits = append(verdict.hits, rows[i].Hit)
		}
	}
	return verdict, nil
}

// nearDuplicatesBefore judges the write on its own view before the commit:
// the commit's state may hold no catalog (a bucket store lends none), and a
// view opened inside the commit is the deadlock Read warns about.
func (e *enforcer) nearDuplicatesBefore(ctx context.Context, req backend.WriteRequest) (verdict nearVerdict, err error) {
	view, err := e.OpenReadView(ctx)
	if err != nil {
		return nearVerdict{}, fmt.Errorf("near-duplicate check: open view: %w", err)
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			verdict.notes = append(verdict.notes, "near-duplicate check: close view: "+closeErr.Error())
		}
	}()
	return e.judgeNearDuplicate(ctx, view, subjectOf(req))
}

// settleNearDuplicate turns a judgment into the write's fate under the mode:
// refused on hits in block mode, noted in warn mode.
func (e *enforcer) settleNearDuplicate(verdict nearVerdict) ([]string, error) {
	if len(verdict.hits) == 0 {
		return verdict.notes, nil
	}
	if e.options.Content.NearDuplicate == contentcheck.Warn {
		return append(verdict.notes, neardup.Note(verdict.hits)), nil
	}
	return nil, &NearDuplicateError{Hits: verdict.hits}
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
