package promote

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/protocol"
)

// DedupLimit is the lookup limit of a dedup: a page that fills may hide a
// candidate beyond it, so a full one is an error, not an answer.
const DedupLimit = 50

// ErrModeUnsupported is a lookup mode the server predates: the client
// validated the mode, so a bad-request on one is a server without it.
var ErrModeUnsupported = errors.New("lookup mode not offered by this server")

// Search is how a dry run looks the target's store up for documents the
// draft repeats.
type Search struct {
	// Lookup sends one lookup; the surface fills host and token and words a
	// transport failure. Scope and limit are the search's.
	Lookup func(ctx context.Context, req fetch.LookupRequest) (fetch.Result, error)
	// CatalogNote is the surface's wording for a body lookup the server
	// answered from the catalog alone.
	CatalogNote string
}

// Dedup fills check's candidates and related documents: the title in body
// mode, the subject tags when that found nothing, then the near-duplicate
// mode. An error is a lookup that cannot be trusted: the dedup is incomplete.
func (s *Search) Dedup(ctx context.Context, check *Check) error {
	if err := s.byTitleThenTags(ctx, check); err != nil {
		return err
	}
	return s.similar(ctx, check)
}

func (s *Search) byTitleThenTags(ctx context.Context, check *Check) error {
	title := check.Draft.Title()
	if title == "" {
		check.Notes = append(check.Notes, "no H1: dedup by title skipped")
	} else {
		rows, catalogOnly, err := s.rows(ctx, fetch.LookupRequest{Query: title, Match: fetch.MatchBody})
		if err != nil {
			return err
		}
		if catalogOnly {
			check.Notes = append(check.Notes, "catalog-only: "+s.CatalogNote)
		}
		check.Candidates, check.Related = Candidates(rows, &check.Draft, false)
	}
	if len(check.Candidates) > 0 {
		return nil
	}
	tags := check.Draft.SubjectTags()
	if len(tags) == 0 {
		return nil
	}
	rows, _, err := s.rows(ctx, fetch.LookupRequest{Query: strings.Join(tags[:min(3, len(tags))], " ")})
	if err != nil {
		return err
	}
	var related []string
	check.Candidates, related = Candidates(rows, &check.Draft, true)
	for _, r := range related {
		if !slices.Contains(check.Related, r) {
			check.Related = append(check.Related, r)
		}
	}
	return nil
}

// similar asks the store which documents the draft repeats, the question
// the write gate answers after a publish. An older server refuses the
// mode: the dry run says so and stands on the title and tag lookups.
func (s *Search) similar(ctx context.Context, check *Check) error {
	// No query: the draft is the input, and a long H1 would be refused
	// before the mode is judged.
	rows, catalogOnly, err := s.rows(ctx, fetch.LookupRequest{Match: fetch.MatchSimilar, Body: check.Draft.Body})
	switch {
	case errors.Is(err, ErrModeUnsupported):
		check.Notes = append(check.Notes, "near-duplicate lookup not offered by this server; dedup by title and tags only")
		return nil
	case err != nil:
		return err
	case catalogOnly:
		check.Notes = append(check.Notes, "near-duplicate lookup unanswered: "+s.CatalogNote)
		return nil
	}
	// An update is not a duplicate of itself.
	rows = slices.DeleteFunc(rows, func(r lookuptable.Row) bool { return r.Path == check.Draft.Path })
	check.Candidates, check.Related = SimilarCandidates(check.Candidates, check.Related, rows)
	return nil
}

// rows is one lookup over the whole store, bounded by DedupLimit.
func (s *Search) rows(ctx context.Context, req fetch.LookupRequest) (rows []lookuptable.Row, catalogOnly bool, err error) {
	req.Scope, req.Limit = "/", DedupLimit
	r, err := s.Lookup(ctx, req)
	switch {
	case err != nil:
		return nil, false, err
	case req.Match != "" && req.Match != fetch.MatchCatalog && r.Response.Status == protocol.StatusBadRequest:
		return nil, false, ErrModeUnsupported
	case r.Response.Status != protocol.StatusOK:
		return nil, false, fmt.Errorf("lookup %q: %s", req.Query, r.Response.Status)
	}
	table, err := lookuptable.ParseTable(r.Response.Body)
	if err != nil {
		return nil, false, err
	}
	if len(table.Rows) >= DedupLimit {
		return nil, false, fmt.Errorf("lookup %q: %d or more matches; narrow the draft's title or tags", req.Query, DedupLimit)
	}
	return table.Rows, fetch.AnsweredFromCatalog(req, r), nil
}
