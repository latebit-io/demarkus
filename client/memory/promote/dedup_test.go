package promote_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/memory/promote"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

const dedupDraft = "# Retry Rules\n\nWhen to retry a call.\n"

func dedupCheck() *promote.Check {
	return promote.Examine(&promote.Draft{Path: "/docs/retry-rules.md", Body: dedupDraft, Metadata: map[string]string{"tags": "retry"}}, nil)
}

// The search owns the dedup's shape: the whole store, a bounded page, the
// title first and the near-duplicate mode last.
func TestDedupLooksTheWholeStoreUpWithinTheLimit(t *testing.T) {
	var seen []fetch.LookupRequest
	search := promote.Search{CatalogNote: "NOTE", Lookup: func(_ context.Context, req fetch.LookupRequest) (fetch.Result, error) {
		seen = append(seen, req)
		if req.Match == fetch.MatchBody {
			return fetchtest.Lookup(req.Query, req.Scope, "", render.LookupRow{Path: "/docs/retries.md", Title: "Retry Rules", Tags: []string{"retry"}}), nil
		}
		return fetchtest.Lookup(req.Query, req.Scope, req.Match), nil
	}}
	check := dedupCheck()
	if err := search.Dedup(t.Context(), check); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Query != "Retry Rules" || seen[0].Match != fetch.MatchBody || seen[1].Match != fetch.MatchSimilar || seen[1].Body != dedupDraft {
		t.Fatalf("lookups = %+v, want the title in body mode then the draft in similar mode", seen)
	}
	for _, req := range seen {
		if req.Scope != "/" || req.Limit != promote.DedupLimit {
			t.Errorf("lookup scope %q limit %d, want / and %d", req.Scope, req.Limit, promote.DedupLimit)
		}
	}
	if len(check.Candidates) != 1 || check.Candidates[0].Path != "/docs/retries.md" {
		t.Errorf("candidates = %+v, want the same-title document", check.Candidates)
	}
	// The body lookup was answered from the catalog: said in the surface's words.
	if notes := strings.Join(check.Notes, "\n"); !strings.Contains(notes, "catalog-only: NOTE") {
		t.Errorf("notes = %q", check.Notes)
	}
}

func TestDedupOutcomesThatAreNotAnAnswer(t *testing.T) {
	answer := func(status string, err error) promote.Search {
		return promote.Search{Lookup: func(_ context.Context, req fetch.LookupRequest) (fetch.Result, error) {
			if req.Match == fetch.MatchSimilar {
				return fetch.Result{Response: protocol.Response{Status: status}}, err
			}
			return fetchtest.Lookup(req.Query, req.Scope, req.Match), nil
		}}
	}
	// An older server refuses the mode: noted, and the dedup stands.
	older, check := answer(protocol.StatusBadRequest, nil), dedupCheck()
	if err := older.Dedup(t.Context(), check); err != nil || !strings.Contains(strings.Join(check.Notes, "\n"), "not offered by this server") {
		t.Errorf("older server: err %v, notes %q", err, check.Notes)
	}
	failing := answer(protocol.StatusServerError, nil)
	if err := failing.Dedup(t.Context(), dedupCheck()); err == nil || errors.Is(err, promote.ErrModeUnsupported) {
		t.Errorf("a failed lookup = %v, want the dedup left incomplete", err)
	}
	// A page that fills may hide a candidate beyond it.
	full := promote.Search{Lookup: func(_ context.Context, req fetch.LookupRequest) (fetch.Result, error) {
		rows := make([]render.LookupRow, promote.DedupLimit)
		for i := range rows {
			rows[i] = render.LookupRow{Path: "/d/" + strings.Repeat("x", i+1) + ".md", Title: "T"}
		}
		return fetchtest.Lookup(req.Query, req.Scope, req.Match, rows...), nil
	}}
	if err := full.Dedup(t.Context(), dedupCheck()); err == nil || !strings.Contains(err.Error(), "or more matches") {
		t.Errorf("a full page = %v, want it refused as untrustworthy", err)
	}
}
